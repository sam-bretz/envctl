package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const ClaudeVersion = workflow.ClaudeHarnessVersion
const ClaudeBinary = "/opt/envctl/harnesses/claude/" + ClaudeVersion + "/claude"
const ClaudeArmSHA = "116fd031f939ef1e09edf170d62c489e1cc28ed6bfbda49f948773ba168c8f62"
const ClaudeAMD64SHA = "9691a2b7bd796712ca8cffb8e32e54ff7fc45b662540233171a16a94a0425653"
const claudeDownload = "https://storage.googleapis.com/claude-code-dist-86c565f3-f756-42ad-8dfa-d59b1c096819/claude-code-releases"

type Claude struct{ Guest guestjob.Client }

const claudeCredentialHelp = "set ANTHROPIC_API_KEY, or run `claude setup-token` and set CLAUDE_CODE_OAUTH_TOKEN"

// ClaudeCredential accepts only non-rotating credentials. A claude.ai login
// session (keychain or ~/.claude/.credentials.json) carries a refresh token
// that rotates on use: copying it into guests lets any one refresh revoke
// every other copy, including the host's own login.
func ClaudeCredential(root, reference string) (Credential, error) {
	if reference == "" {
		if os.Getenv("ANTHROPIC_API_KEY") != "" {
			reference = "env:ANTHROPIC_API_KEY"
		} else if os.Getenv("CLAUDE_CODE_OAUTH_TOKEN") != "" {
			reference = "oauth-env:CLAUDE_CODE_OAUTH_TOKEN"
		} else {
			return Credential{}, errors.New("Claude connection is not ready: " + claudeCredentialHelp)
		}
	}
	if strings.HasPrefix(reference, "env:") || strings.HasPrefix(reference, "oauth-env:") {
		kind, name, _ := strings.Cut(reference, ":")
		value := os.Getenv(name)
		if value == "" {
			return Credential{}, errors.New("Claude credential environment reference is unavailable")
		}
		c := Credential{Secrets: []string{value}}
		if kind == "oauth-env" {
			c.OAuthToken = value
		} else {
			c.APIKey = value
		}
		return c, nil
	}
	if !strings.HasPrefix(reference, "file:") {
		return Credential{}, errors.New("Claude credentials require env:, oauth-env:, or file: references; " + claudeCredentialHelp)
	}
	filename := strings.TrimPrefix(reference, "file:")
	if !filepath.IsAbs(filename) {
		filename = filepath.Join(root, filename)
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return Credential{}, errors.New("Claude credential store is unavailable")
	}
	var doc struct {
		APIKey     string          `json:"ANTHROPIC_API_KEY"`
		OAuthToken string          `json:"CLAUDE_CODE_OAUTH_TOKEN"`
		Session    json.RawMessage `json:"claudeAiOauth"`
	}
	if len(raw) > 1<<20 || json.Unmarshal(raw, &doc) != nil {
		return Credential{}, errors.New("Claude credential file must contain a valid credential object")
	}
	if len(doc.Session) > 0 {
		return Credential{}, errors.New("Claude credential file is a refreshable claude.ai login session, which cannot be shared with guests; " + claudeCredentialHelp)
	}
	c := Credential{APIKey: doc.APIKey, OAuthToken: doc.OAuthToken}
	for _, value := range []string{doc.APIKey, doc.OAuthToken} {
		if value != "" {
			c.Secrets = append(c.Secrets, value)
		}
	}
	if c.APIKey == "" && c.OAuthToken == "" {
		return Credential{}, errors.New("Claude credential file contains no supported credentials; " + claudeCredentialHelp)
	}
	return c, nil
}

func (c Claude) Install(ctx context.Context) error {
	return c.Guest.Provider.Exec(ctx, c.Guest.Runtime, vm.Command{Args: []string{"sudo", "bash", "-c", `set -euo pipefail
version="$1"; arm="$2"; amd="$3"; base="$4"
destination="/opt/envctl/harnesses/claude/$version"
case "$(uname -m)" in aarch64) target=linux-arm64; digest="$arm";; x86_64) target=linux-x64; digest="$amd";; *) exit 1;; esac
install -d -m 755 /opt/envctl/harnesses/claude
if [ ! -e "$destination" ]; then
 stage=$(mktemp -d /opt/envctl/harnesses/claude/.install.XXXXXX)
 trap 'rm -rf "$stage"' EXIT
 curl --fail --silent --show-error --location --retry 3 --connect-timeout 20 --max-time 600 "$base/$version/$target/claude" -o "$stage/claude"
 printf '%s  %s\n' "$digest" "$stage/claude" | sha256sum --status -c -
 chmod 755 "$stage" "$stage/claude"
 mv "$stage" "$destination"
fi
printf '%s  %s\n' "$digest" "$destination/claude" | sha256sum --status -c -
chown root:root "$destination" "$destination/claude"
chmod 755 "$destination" "$destination/claude"
`, "envctl-claude-install", ClaudeVersion, ClaudeArmSHA, ClaudeAMD64SHA, claudeDownload}})
}

func claudeHome(role string) string { return "/work/envctl/harness-homes/claude-" + role }

func (c Claude) Request(i Invocation, credential Credential) (guestjob.Request, error) {
	if err := validateInvocation(i); err != nil {
		return guestjob.Request{}, err
	}
	dir := invocationDir(i.ID)
	args := []string{"python3", "-c", claudeProcess, dir + "/schema.json", dir + "/result.json", ClaudeBinary, "--print", "--verbose", "--output-format", "stream-json", "--safe-mode", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`, "--dangerously-skip-permissions", "--tools", "Bash,Read,Edit,Write,Glob,Grep"}
	if i.Model != "" {
		args = append(args, "--model", i.Model)
	}
	if i.Session != "" {
		args = append(args, "--resume", i.Session)
	}
	env := map[string]string{"CLAUDE_CONFIG_DIR": claudeHome(i.Role), "DISABLE_AUTOUPDATER": "1", "PYTHONDONTWRITEBYTECODE": "1"}
	if credential.APIKey != "" {
		env["ANTHROPIC_API_KEY"] = credential.APIKey
	}
	if credential.OAuthToken != "" {
		env["CLAUDE_CODE_OAUTH_TOKEN"] = credential.OAuthToken
	}
	return guestjob.Request{ID: i.ID, Args: args, Dir: i.Directory, Env: withEnv(env, i), Input: i.Prompt, Secrets: credential.Secrets, TimeoutSeconds: i.TimeoutSeconds}, nil
}

func (c Claude) Start(ctx context.Context, i Invocation, credential Credential) (guestjob.Status, error) {
	req, err := c.Request(i, credential)
	if err != nil {
		return guestjob.Status{}, err
	}
	if credential.APIKey == "" && credential.OAuthToken == "" {
		return guestjob.Status{}, errors.New("Claude connection is not ready: " + claudeCredentialHelp)
	}
	schema, err := json.Marshal(i.Schema)
	if err != nil {
		return guestjob.Status{}, err
	}
	if err = writePrivate(ctx, c.Guest, invocationDir(i.ID)+"/schema.json", schema, true); err != nil {
		return guestjob.Status{}, err
	}
	if err = writePrivate(ctx, c.Guest, claudeHome(i.Role)+"/.claude.json", []byte(`{"hasCompletedOnboarding":true}`), false); err != nil {
		return guestjob.Status{}, err
	}
	return c.Guest.Submit(ctx, req)
}

func (c Claude) Result(ctx context.Context, id string) (json.RawMessage, error) {
	return readResult(ctx, c.Guest, id)
}
func (c Claude) Session(stream string) string {
	for _, line := range strings.Split(stream, "\n") {
		var event struct {
			Type    string `json:"type"`
			Subtype string `json:"subtype"`
			Session string `json:"session_id"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.Type == "system" && event.Subtype == "init" && sessionID.MatchString(event.Session) {
			return event.Session
		}
	}
	return ""
}

// Preserve the native event stream for progress/reconnect. Only a successful
// terminal structured result is written to the shared harness result contract.
const claudeProcess = `import json,os,pathlib,subprocess,sys,tempfile
schema,result=sys.argv[1:3]
args=sys.argv[3:]+['--json-schema',open(schema).read()]
p=subprocess.Popen(args,stdin=sys.stdin,stdout=subprocess.PIPE,stderr=sys.stderr)
final=None
for line in p.stdout:
 sys.stdout.buffer.write(line);sys.stdout.buffer.flush()
 try:
  event=json.loads(line)
  if event.get('type')=='result':final=event
 except (ValueError,AttributeError):pass
code=p.wait()
if code or not final or final.get('is_error') or final.get('subtype')!='success' or not isinstance(final.get('structured_output'),dict):sys.exit(code or 1)
path=pathlib.Path(result)
fd,tmp=tempfile.mkstemp(dir=path.parent)
try:
 with os.fdopen(fd,'w') as f:json.dump(final['structured_output'],f);f.flush();os.fsync(f.fileno())
 os.replace(tmp,path)
 fd=os.open(path.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
finally:
 if os.path.exists(tmp):os.unlink(tmp)
`
