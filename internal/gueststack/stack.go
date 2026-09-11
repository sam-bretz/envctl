// Package gueststack runs Compose entirely within an execution VM. The host
// only transforms the normalized model; it never loads repository paths or
// invokes its own Docker daemon.
package gueststack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"github.com/sam-bretz/envctl/internal/compose"
	"github.com/sam-bretz/envctl/internal/dockerx"
	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/manifest"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

var identity = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,100}$`)

type Spec struct {
	ID      string            `json:"id"`      // immutable preparation identity, normally an attempt ID
	Project string            `json:"project"` // stable across serial stages of a revision
	Root    string            `json:"root"`    // guest repository/worktree containing Compose inputs
	Stack   manifest.Stack    `json:"stack"`
	Expose  []manifest.Expose `json:"expose"`
}
type Prepared struct {
	Spec     Spec     `json:"spec"`
	File     string   `json:"file"`
	Digest   string   `json:"digest"`
	Services []string `json:"services"`
}
type Client struct {
	Executor guestjob.Executor
	Runtime  string
}

// The default guest is pinned Ubuntu 26.04. Buildx is required by Compose for
// application builds, even when an image-only smoke stack did not exercise it.
const BuildxPackageVersion = "0.30.1-0ubuntu1"

func (c Client) EnsureBuildTools(ctx context.Context) error {
	script := `set -eu
version="$1"
current=$(dpkg-query -W -f='${Version}' docker-buildx 2>/dev/null || true)
if [ "$current" != "$version" ]; then
 export DEBIAN_FRONTEND=noninteractive
 apt-get -o Acquire::Retries=3 -o Acquire::ForceIPv4=true install -y --no-install-recommends "docker-buildx=$version" >/dev/null
fi
docker buildx version >/dev/null
install -d -m 700 /var/lib/envctl
printf '%s\n' "docker-buildx=$version" > /var/lib/envctl/buildx.lock
`
	_, err := c.exec(ctx, "build tool preparation", []string{"sudo", "sh", "-c", script, "envctl-buildx", BuildxPackageVersion}, nil)
	return err
}

func workflowBytesDigest(raw []byte) string { h := sha256.Sum256(raw); return hex.EncodeToString(h[:]) }

// Output is intentionally bounded. Compose may interpolate secrets; none of
// its raw diagnostics are permitted into API errors, events, or logs.
type output struct{ bytes.Buffer }

func (b *output) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("guest Compose response too large")
	}
	return b.Buffer.Write(p)
}
func (c Client) exec(ctx context.Context, operation string, args []string, input io.Reader) ([]byte, error) {
	var out output
	if err := c.Executor.Exec(ctx, c.Runtime, vm.Command{Args: args, Stdin: input, Stdout: &out, Stderr: io.Discard}); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("guest Compose %s failed; inspect the scoped stack inside its VM", operation)
	}
	return out.Bytes(), nil
}
func guestPath(p string) bool {
	return path.IsAbs(p) && path.Clean(p) == p && strings.HasPrefix(p, "/work/envctl/") && !strings.ContainsAny(p, "\x00\r\n")
}
func (s Spec) validate() error {
	if !identity.MatchString(s.ID) || !identity.MatchString(s.Project) || !guestPath(s.Root) {
		return errors.New("invalid guest stack identity or root")
	}
	if len(s.Stack.Files) == 0 {
		return errors.New("workflow requires at least one guest Compose file")
	}
	for _, files := range [][]string{s.Stack.Files, s.Stack.EnvFiles} {
		for _, f := range files {
			if f == "" || path.IsAbs(f) || strings.ContainsAny(f, "\x00\r\n") || !guestPath(path.Join(s.Root, f)) {
				return errors.New("Compose inputs must resolve within guest workspace storage")
			}
		}
	}
	return nil
}
func (s Spec) dir() string { return "/var/lib/envctl/stacks/" + s.ID }
func dockerArgs(root string) []string {
	// Ignore the guest login user's Docker contexts and environment too.
	return []string{"sudo", "env", "-i", "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/root", "DOCKER_HOST=unix:///var/run/docker.sock", "docker", "compose", "--ansi", "never", "--project-directory", root}
}
func (p Prepared) command(args ...string) []string {
	cmd := append(dockerArgs(p.Spec.Root), "--project-name", p.Spec.Project, "-f", p.File)
	return append(cmd, args...)
}

// Prepare freezes the first successful normalization and transformation. Lost
// acknowledgements replay the existing receipt instead of rereading changed
// source files, environment values, or tags. File and receipt publish together.
func (c Client) Prepare(ctx context.Context, s Spec) (Prepared, error) {
	if err := s.validate(); err != nil {
		return Prepared{}, err
	}
	binding := workflow.Digest(s)
	read := `import json,pathlib,hashlib,sys
p=pathlib.Path(sys.argv[1]); binding=sys.argv[2]
if not p.exists(): print('null'); sys.exit(0)
r=json.loads((p/'receipt.json').read_text())
assert r['binding']==binding
assert hashlib.sha256((p/'compose.yaml').read_bytes()).hexdigest()==r['prepared']['digest']
print(json.dumps(r['prepared']))`
	raw, err := c.exec(ctx, "receipt", []string{"sudo", "python3", "-c", read, s.dir(), binding}, nil)
	if err != nil {
		return Prepared{}, err
	}
	var previous *Prepared
	if json.Unmarshal(raw, &previous) != nil {
		return Prepared{}, errors.New("invalid guest stack receipt")
	}
	if previous != nil {
		return *previous, nil
	}
	cmd := append(dockerArgs(s.Root), "--project-name", s.Project)
	for _, f := range s.Stack.Files {
		cmd = append(cmd, "-f", path.Join(s.Root, f))
	}
	for _, f := range s.Stack.EnvFiles {
		cmd = append(cmd, "--env-file", path.Join(s.Root, f))
	}
	for _, profile := range s.Stack.Profiles {
		cmd = append(cmd, "--profile", profile)
	}
	cmd = append(cmd, "config", "--format", "json")
	raw, err = c.exec(ctx, "normalize", cmd, nil)
	if err != nil {
		return Prepared{}, err
	}
	rendered, services, err := transform(raw, s)
	if err != nil {
		return Prepared{}, err
	}
	p := Prepared{Spec: s, File: s.dir() + "/compose.yaml", Digest: workflowBytesDigest(rendered), Services: services}
	payload, _ := json.Marshal(map[string]any{"binding": binding, "prepared": p, "compose": string(rendered)})
	write := `import json,pathlib,os,sys,tempfile,shutil,hashlib
p=pathlib.Path(sys.argv[1]); data=json.load(sys.stdin); raw=data.pop('compose').encode()
assert hashlib.sha256(raw).hexdigest()==data['prepared']['digest']
p.parent.mkdir(parents=True,exist_ok=True,mode=0o700)
if p.exists():
 assert json.loads((p/'receipt.json').read_text())==data
 assert (p/'compose.yaml').read_bytes()==raw
 sys.exit(0)
stage=pathlib.Path(tempfile.mkdtemp(prefix='.preparing-',dir=p.parent))
try:
 for name,content in [('compose.yaml',raw),('receipt.json',json.dumps(data).encode())]:
  with (stage/name).open('wb') as f:
   os.chmod(stage/name,0o600); f.write(content); f.flush(); os.fsync(f.fileno())
 fd=os.open(stage,os.O_RDONLY); os.fsync(fd); os.close(fd)
 os.rename(stage,p)
 fd=os.open(p.parent,os.O_RDONLY); os.fsync(fd); os.close(fd)
finally:
 if stage.exists(): shutil.rmtree(stage)`
	_, err = c.exec(ctx, "freeze", []string{"sudo", "python3", "-c", write, s.dir()}, bytes.NewReader(payload))
	return p, err
}

// loader.Transform decodes canonical data only. Unlike Load/ModelToProject it
// does not resolve env_file, label_file, include, or any other host filesystem
// resource. All interpolation and path normalization already happened in VM.
func transform(raw []byte, s Spec) ([]byte, []string, error) {
	var model map[string]any
	if json.Unmarshal(raw, &model) != nil {
		return nil, nil, errors.New("invalid normalized guest Compose model")
	}
	var project types.Project
	if loader.Transform(model, &project) != nil {
		return nil, nil, errors.New("cannot decode normalized guest Compose model")
	}
	if len(project.Services) == 0 {
		return nil, nil, errors.New("guest Compose model has no active services")
	}
	for _, ex := range s.Expose {
		svc, ok := project.Services[ex.Service]
		if !ok || ex.Port < 1 || ex.Port > 65535 {
			return nil, nil, errors.New("exposed service or port is invalid")
		}
		found := false
		for _, p := range svc.Ports {
			if int(p.Target) == ex.Port && p.Protocol != "udp" {
				found = true
			}
		}
		if !found {
			svc.Ports = append(svc.Ports, types.ServicePortConfig{Target: uint32(ex.Port), Protocol: "tcp"})
		}
		project.Services[ex.Service] = svc
	}
	for _, svc := range project.Services {
		if len(svc.EnvFiles) > 0 || len(svc.LabelFiles) > 0 {
			return nil, nil, errors.New("guest Compose model contains unresolved file references")
		}
		for _, vol := range svc.Volumes {
			if vol.Type == "bind" && !guestPath(vol.Source) {
				return nil, nil, errors.New("Compose bind sources must be in guest workspace storage")
			}
		}
		if svc.Build != nil && !guestPath(svc.Build.Context) {
			return nil, nil, errors.New("Compose build contexts must be prepared in guest workspace storage")
		}
	}
	rendered, result, err := compose.Transform(&project, compose.Options{Project: s.Project, Env: s.ID, Backend: "local-vm", Mode: compose.ModeGuest})
	if err != nil {
		return nil, nil, errors.New("guest Compose transformation failed")
	}
	return rendered, result.Services, nil
}

func (c Client) Up(ctx context.Context, p Prepared, waitSeconds int) error {
	if err := p.validate(); err != nil {
		return err
	}
	if waitSeconds < 1 {
		return errors.New("Compose health timeout must be positive")
	}
	if err := c.EnsureBuildTools(ctx); err != nil {
		return err
	}
	_, err := c.exec(ctx, "up", p.command("up", "--detach", "--build", "--wait", "--wait-timeout", strconv.Itoa(waitSeconds)), nil)
	return err
}
func (c Client) Status(ctx context.Context, p Prepared) ([]dockerx.PSEntry, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	raw, err := c.exec(ctx, "status", p.command("ps", "--all", "--format", "json"), nil)
	if err != nil {
		return nil, err
	}
	entries, err := dockerx.ParsePS(string(raw))
	if err != nil {
		return nil, errors.New("invalid guest Compose status")
	}
	return entries, nil
}
func (c Client) Down(ctx context.Context, p Prepared, volumes bool) error {
	if err := p.validate(); err != nil {
		return err
	}
	args := []string{"down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	_, err := c.exec(ctx, "down", p.command(args...), nil)
	return err
}

// Container resolves exactly one live service instance under this prepared
// Compose project. Dataset operations never accept an arbitrary host container.
func (c Client) Container(ctx context.Context, p Prepared, service string) (string, error) {
	if err := p.validate(); err != nil {
		return "", err
	}
	if !slices.Contains(p.Services, service) {
		return "", errors.New("dataset service is not in the prepared stack")
	}
	raw, err := c.exec(ctx, "service identity", p.command("ps", "--quiet", service), nil)
	if err != nil {
		return "", err
	}
	ids := strings.Fields(string(raw))
	if len(ids) != 1 || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(ids[0]) {
		return "", errors.New("dataset service must have exactly one running container")
	}
	return ids[0], nil
}

func (c Client) SetWritersRunning(ctx context.Context, p Prepared, services []string, running bool) error {
	if err := p.validate(); err != nil {
		return err
	}
	for _, service := range services {
		if !slices.Contains(p.Services, service) {
			return errors.New("quiescence references an unavailable service")
		}
	}
	if len(services) == 0 {
		return nil
	}
	op := []string{"stop"}
	if running {
		op = []string{"start"}
	}
	_, err := c.exec(ctx, "writer quiescence", p.command(append(op, services...)...), nil)
	if err != nil || !running {
		return err
	}
	wait, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	for {
		entries, err := c.Status(wait, p)
		if err != nil {
			return err
		}
		ready := map[string]bool{}
		for _, entry := range entries {
			if entry.State == "running" && (entry.Health == "" || entry.Health == "healthy") {
				ready[entry.Service] = true
			}
		}
		complete := true
		for _, service := range services {
			if !ready[service] {
				complete = false
			}
		}
		if complete {
			return nil
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-wait.Done():
			timer.Stop()
			return errors.New("quiesced writers did not recover health")
		case <-timer.C:
		}
	}
}
func (p Prepared) validate() error {
	if err := p.Spec.validate(); err != nil {
		return err
	}
	if p.File != p.Spec.dir()+"/compose.yaml" {
		return errors.New("invalid prepared Compose path")
	}
	return nil
}

// Healthy requires evidence for every expected service; an empty ps response
// or a starting/unhealthy replica never counts as readiness.
func Healthy(p Prepared, entries []dockerx.PSEntry) bool {
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.State != "running" || (entry.Health != "" && entry.Health != "healthy") {
			return false
		}
		seen[entry.Service] = true
	}
	if len(p.Services) == 0 {
		return false
	}
	for _, service := range p.Services {
		if !seen[service] {
			return false
		}
	}
	return true
}
