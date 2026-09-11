// Package agent adapts harness protocols to durable guest jobs. Harness output
// is a proposal: commits, tests, artifacts, and checkpoint admission are verified
// by the execution backend and coordinator, outside the harness conversation.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const CodexVersion = workflow.CodexHarnessVersion
const CodexBinary = "/opt/envctl/harnesses/codex/" + CodexVersion + "/codex"
const CodexArmSHA = "583b48df32804213bdcd338c2e5adb06b34340821fa757a726cc0a524fa33c27"
const CodexAMD64SHA = "d7e18b2597ae8f242f5f31ee9e90deef48dbc9edd634d9868fb6435d08c07f02"
const CodexHostArmSHA = "20aefa302c2022b496e32911bf954a5f76c7fd749c6bdb9fbd711e32b66dcbfa"
const CodexHostAMD64SHA = "a68df7cca23c6da7cde175677df7de61c73a234add1333a1254b86d641af01f7"

var safeID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,100}$`)
var sessionID = regexp.MustCompile(`^[a-f0-9-]{36}$`)

type Credential struct {
	AuthJSON   []byte
	APIKey     string
	OAuthToken string
	Secrets    []string
}

// CodexCredential resolves one explicit reference, or the standard local Codex
// connection. Secret values are never serialized into run configuration.
func CodexCredential(root, reference string) (Credential, error) {
	if reference == "" {
		if value := os.Getenv("OPENAI_API_KEY"); value != "" {
			return Credential{APIKey: value, Secrets: []string{value}}, nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return Credential{}, err
		}
		reference = "file:" + filepath.Join(home, ".codex", "auth.json")
	}
	if strings.HasPrefix(reference, "env:") {
		value := os.Getenv(strings.TrimPrefix(reference, "env:"))
		if value == "" {
			return Credential{}, errors.New("Codex credential environment reference is unavailable")
		}
		return Credential{APIKey: value, Secrets: []string{value}}, nil
	}
	if !strings.HasPrefix(reference, "file:") {
		return Credential{}, errors.New("Codex credentials require an env: or file: reference")
	}
	filename := strings.TrimPrefix(reference, "file:")
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(root, filename)
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return Credential{}, errors.New("Codex auth-file reference is unavailable")
	}
	var doc map[string]any
	if len(raw) > 1<<20 || json.Unmarshal(raw, &doc) != nil {
		return Credential{}, errors.New("Codex auth file must contain a valid credential object")
	}
	credential := Credential{AuthJSON: raw}
	if key, ok := doc["OPENAI_API_KEY"].(string); ok && key != "" {
		credential.Secrets = append(credential.Secrets, key)
	}
	if tokens, ok := doc["tokens"].(map[string]any); ok {
		for _, name := range []string{"access_token", "refresh_token", "id_token"} {
			if token, ok := tokens[name].(string); ok && token != "" {
				credential.Secrets = append(credential.Secrets, token)
			}
		}
	}
	if len(credential.Secrets) == 0 {
		return Credential{}, errors.New("Codex auth file contains no supported credentials")
	}
	return credential, nil
}

type Invocation struct {
	ID             string
	Role           string
	Directory      string
	Prompt         string
	Schema         map[string]any
	Model          string
	Session        string
	TimeoutSeconds int
	// Env carries coordinator-derived ENVCTL_* values, such as guest service
	// endpoints. It cannot override harness homes or credentials.
	Env map[string]string `json:",omitempty"`
}
type Codex struct{ Guest guestjob.Client }

var invocationEnvKey = regexp.MustCompile(`^ENVCTL_[A-Z0-9_]{1,100}$`)

// withEnv adds validated invocation environment without replacing harness
// entries; validateInvocation already restricts keys to the ENVCTL_ prefix.
func withEnv(env map[string]string, i Invocation) map[string]string {
	for k, v := range i.Env {
		if _, reserved := env[k]; !reserved {
			env[k] = v
		}
	}
	return env
}

func (c Codex) Install(ctx context.Context) error {
	// Archive pins come from the official release-assets metadata. Installation
	// records the extracted binary's checksum for drift detection on retries.
	args := []string{"sudo", "bash", "-c", `set -euo pipefail
version="$1"; arm="$2"; amd="$3"; host_arm="$4"; host_amd="$5"
destination="/opt/envctl/harnesses/codex/$version"
case "$(uname -m)" in
  aarch64) target=aarch64-unknown-linux-musl; digest="$arm"; host_digest="$host_arm";;
  x86_64) target=x86_64-unknown-linux-musl; digest="$amd"; host_digest="$host_amd";;
  *) echo 'unsupported harness architecture' >&2; exit 1;;
esac
install -d -m 755 /opt/envctl/harnesses/codex
if [ -f "$destination/binary.sha256" ]; then
 (cd "$destination" && sha256sum --status -c binary.sha256) || { echo 'installed Codex binary changed' >&2; exit 1; }
else
stage=$(mktemp -d /opt/envctl/harnesses/codex/.install.XXXXXX)
trap 'rm -rf "$stage"' EXIT
curl --fail --silent --show-error --location --retry 3 --connect-timeout 20 --max-time 600 "https://github.com/openai/codex/releases/download/rust-v$version/codex-$target.tar.gz" -o "$stage/archive.tar.gz"
printf '%s  %s\n' "$digest" "$stage/archive.tar.gz" | sha256sum --status -c -
tar -xzf "$stage/archive.tar.gz" -C "$stage" "codex-$target"
mv "$stage/codex-$target" "$stage/codex"
chmod 755 "$stage" "$stage/codex"
rm "$stage/archive.tar.gz"
(cd "$stage" && sha256sum codex > binary.sha256)
printf '%s\n' "$digest" > "$stage/archive.sha256"
test ! -e "$destination"
mv "$stage" "$destination"
fi
if [ -f "$destination/code-mode-host.sha256" ]; then
 (cd "$destination" && sha256sum --status -c code-mode-host.sha256) || { echo 'installed Codex tool runtime changed' >&2; exit 1; }
else
 stage=$(mktemp -d /opt/envctl/harnesses/codex/.host-install.XXXXXX)
 trap 'rm -rf "$stage"' EXIT
 curl --fail --silent --show-error --location --retry 3 --connect-timeout 20 --max-time 600 "https://github.com/openai/codex/releases/download/rust-v$version/codex-code-mode-host-$target.tar.gz" -o "$stage/archive.tar.gz"
 printf '%s  %s\n' "$host_digest" "$stage/archive.tar.gz" | sha256sum --status -c -
 tar -xzf "$stage/archive.tar.gz" -C "$stage" "codex-code-mode-host-$target"
 chmod 755 "$stage/codex-code-mode-host-$target"
 mv "$stage/codex-code-mode-host-$target" "$destination/codex-code-mode-host"
 (cd "$destination" && sha256sum codex-code-mode-host > code-mode-host.sha256.tmp && mv code-mode-host.sha256.tmp code-mode-host.sha256)
 printf '%s\n' "$host_digest" > "$destination/code-mode-host-archive.sha256"
fi
chown root:root "$destination/codex" "$destination/codex-code-mode-host"
test -x "$destination/codex" && test -x "$destination/codex-code-mode-host"
`, "envctl-codex-install", CodexVersion, CodexArmSHA, CodexAMD64SHA, CodexHostArmSHA, CodexHostAMD64SHA}
	var diagnostic limitBuffer
	if err := c.Guest.Provider.Exec(ctx, c.Guest.Runtime, vm.Command{Args: args, Stderr: &diagnostic}); err != nil {
		return fmt.Errorf("install pinned Codex guest harness: %w: %s", err, strings.TrimSpace(diagnostic.data.String()))
	}
	return nil
}

func invocationDir(id string) string { return "/work/envctl/agent-jobs/" + id }
func codexHome(role string) string   { return "/work/envctl/harness-homes/codex-" + role }

func validateInvocation(i Invocation) error {
	if !safeID.MatchString(i.ID) || (i.Role != "worker" && i.Role != "supervisor") {
		return errors.New("invalid harness assignment identity")
	}
	if !strings.HasPrefix(path.Clean(i.Directory), "/work/envctl/") || strings.ContainsRune(i.Directory, '\x00') {
		return errors.New("harness workspace must be inside the guest")
	}
	if strings.TrimSpace(i.Prompt) == "" || i.TimeoutSeconds < 1 || i.TimeoutSeconds > 86400 || len(i.Schema) == 0 {
		return errors.New("harness requires a prompt, output schema, and finite attempt budget")
	}
	if i.Session != "" && !sessionID.MatchString(i.Session) {
		return errors.New("invalid harness session ID")
	}
	if len(i.Env) > 128 {
		return errors.New("harness environment exceeds its bound")
	}
	for k, v := range i.Env {
		if !invocationEnvKey.MatchString(k) || len(v) > 1024 || strings.ContainsAny(v, "\x00\r\n") {
			return errors.New("harness environment entries must be bounded ENVCTL_ values")
		}
	}
	return nil
}

func (c Codex) Request(i Invocation, credential Credential) (guestjob.Request, error) {
	if err := validateInvocation(i); err != nil {
		return guestjob.Request{}, err
	}
	dir := invocationDir(i.ID)
	args := []string{CodexBinary, "exec"}
	if i.Session != "" {
		args = append(args, "resume")
	}
	args = append(args, "--json", "--ignore-user-config", "--ignore-rules", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", "--output-schema", dir+"/schema.json", "--output-last-message", dir+"/result.json")
	if i.Model != "" {
		args = append(args, "--model", i.Model)
	}
	if i.Session != "" {
		args = append(args, i.Session)
	}
	args = append(args, "-")
	env := map[string]string{"CODEX_HOME": codexHome(i.Role), "PYTHONDONTWRITEBYTECODE": "1"}
	if credential.APIKey != "" {
		env["CODEX_API_KEY"] = credential.APIKey
	}
	return guestjob.Request{ID: i.ID, Args: args, Dir: i.Directory, Env: withEnv(env, i), Input: i.Prompt, Secrets: credential.Secrets, TimeoutSeconds: i.TimeoutSeconds}, nil
}

func (c Codex) Start(ctx context.Context, i Invocation, credential Credential) (guestjob.Status, error) {
	req, err := c.Request(i, credential)
	if err != nil {
		return guestjob.Status{}, err
	}
	if len(credential.AuthJSON) == 0 && credential.APIKey == "" {
		return guestjob.Status{}, errors.New("Codex connection is not ready: credential reference required")
	}
	schema, err := json.Marshal(i.Schema)
	if err != nil {
		return guestjob.Status{}, err
	}
	if err = c.writePrivate(ctx, invocationDir(i.ID)+"/schema.json", schema, true); err != nil {
		return guestjob.Status{}, err
	}
	if len(credential.AuthJSON) > 0 {
		// Existing guest auth may contain refreshed credentials: do not roll it
		// back on reconnect. Explicit renewal uses a separate lifecycle action.
		if err = c.writePrivate(ctx, codexHome(i.Role)+"/auth.json", credential.AuthJSON, false); err != nil {
			return guestjob.Status{}, err
		}
	} else {
		if err = c.Guest.Provider.Exec(ctx, c.Guest.Runtime, vm.Command{Args: []string{"sudo", "install", "-d", "-o", "envctl-agent", "-g", "envctl-agent", "-m", "700", codexHome(i.Role)}}); err != nil {
			return guestjob.Status{}, errors.New("prepare guest harness home failed")
		}
	}
	return c.Guest.Submit(ctx, req)
}

func (c Codex) writePrivate(ctx context.Context, filename string, data []byte, requireSame bool) error {
	return writePrivate(ctx, c.Guest, filename, data, requireSame)
}

func writePrivate(ctx context.Context, guest guestjob.Client, filename string, data []byte, requireSame bool) error {
	// Content travels through stdin. Only scoped paths enter argv; errors never
	// include credential bytes. File creation and renames are durable.
	mode := "keep"
	if requireSame {
		mode = "same"
	}
	program := `import hashlib,os,pathlib,pwd,sys,tempfile
p=pathlib.Path(sys.argv[1]); mode=sys.argv[2]; data=sys.stdin.buffer.read(1048577)
assert len(data)<=1048576 and p.is_relative_to('/work/envctl')
user=pwd.getpwnam('envctl-agent')
p.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
os.chown(p.parent,user.pw_uid,user.pw_gid)
if p.exists():
 if mode=='same': assert hashlib.sha256(p.read_bytes()).digest()==hashlib.sha256(data).digest()
 sys.exit(0)
fd,tmp=tempfile.mkstemp(dir=p.parent)
try:
 with os.fdopen(fd,'wb') as f:
  f.write(data); f.flush(); os.fsync(f.fileno())
 os.chown(tmp,user.pw_uid,user.pw_gid)
 os.replace(tmp,p)
 fd=os.open(p.parent,os.O_RDONLY); os.fsync(fd); os.close(fd)
finally:
 if os.path.exists(tmp): os.unlink(tmp)
`
	if err := guest.Provider.Exec(ctx, guest.Runtime, vm.Command{Args: []string{"sudo", "python3", "-c", program, filename, mode}, Stdin: bytes.NewReader(data)}); err != nil {
		return errors.New("prepare private guest harness input failed")
	}
	return nil
}

// Result reads only the known output path for this assignment. It accepts one
// JSON object; an agent's prose is not a completion contract.
func (c Codex) Result(ctx context.Context, id string) (json.RawMessage, error) {
	return readResult(ctx, c.Guest, id)
}

func readResult(ctx context.Context, guest guestjob.Client, id string) (json.RawMessage, error) {
	if !safeID.MatchString(id) {
		return nil, errors.New("invalid harness assignment ID")
	}
	var out limitBuffer
	if err := guest.Provider.Exec(ctx, guest.Runtime, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "cat", invocationDir(id) + "/result.json"}, Stdout: &out}); err != nil {
		return nil, errors.New("harness structured result is unavailable")
	}
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(out.data.Bytes()))
	if err := d.Decode(&value); err != nil || value == nil {
		return nil, errors.New("harness result is not a JSON object")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return nil, errors.New("harness returned more than one result")
	}
	return json.RawMessage(out.data.Bytes()), nil
}

type limitBuffer struct{ data bytes.Buffer }

func (b *limitBuffer) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > 1<<20 {
		return 0, errors.New("harness output exceeded limit")
	}
	return b.data.Write(p)
}

// Session extracts the explicit session identity from Codex's structured stream.
// It never selects the globally most recent conversation when resuming.
func Session(stream string) string {
	for _, line := range strings.Split(stream, "\n") {
		var event struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "thread.started" && sessionID.MatchString(event.ThreadID) {
			return event.ThreadID
		}
	}
	return ""
}
