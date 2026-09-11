// Package guestjob owns durable guest processes. A coordinator disconnect does
// not cancel a job: systemd owns its process group and the guest journals results.
package guestjob

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	vm "github.com/sam-bretz/envctl/internal/runtime"
)

//go:embed runner.py
var runner []byte

const RunnerPath = "/opt/envctl/runner.py"

type Request struct {
	ID             string            `json:"id"`
	Args           []string          `json:"args"`
	Dir            string            `json:"dir"`
	Env            map[string]string `json:"env"`
	Input          string            `json:"input"`
	Secrets        []string          `json:"secrets"`
	TimeoutSeconds int               `json:"timeout_seconds"`
}
type Status struct {
	ID        string `json:"id"`
	State     string `json:"state"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Output    string `json:"output,omitempty"`
	Cursor    int64  `json:"cursor"`
	Truncated bool   `json:"truncated"`
}
type Executor interface {
	Exec(context.Context, string, vm.Command) error
}
type Client struct {
	Provider Executor
	Runtime  string
}

func (c Client) Install(ctx context.Context) error {
	// The script and journal are owned by root. Workers run as a distinct user
	// with only the guest's Docker access, never the coordinator's host identity.
	return c.Provider.Exec(ctx, c.Runtime, vm.Command{Args: []string{"sudo", "sh", "-c", `set -eu
id envctl-agent >/dev/null 2>&1 || useradd --create-home --shell /bin/bash envctl-agent
usermod -aG docker envctl-agent
install -d -m 755 /opt/envctl
install -d -m 700 /var/lib/envctl/jobs
install -d -o envctl-agent -g envctl-agent -m 700 /work/envctl
tmp=$(mktemp /opt/envctl/.runner.XXXXXX)
trap 'rm -f "$tmp"' EXIT
cat > "$tmp"
chmod 700 "$tmp"
mv "$tmp" /opt/envctl/runner.py`}, Stdin: bytes.NewReader(runner)})
}
func (c Client) call(ctx context.Context, operation string, body any) (Status, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return Status{}, err
	}
	out := boundedOutput{limit: 1 << 20}
	err = c.Provider.Exec(ctx, c.Runtime, vm.Command{Args: []string{"sudo", "python3", RunnerPath, operation}, Stdin: bytes.NewReader(raw), Stdout: &out, Stderr: io.Discard})
	// Never return transport output here: it may contain a rejected request's
	// credentials. Protocol errors have a bounded, non-secret diagnostic field.
	if err != nil {
		return Status{}, fmt.Errorf("guest job %s transport failed: %w", operation, err)
	}
	var status Status
	dec := json.NewDecoder(&out)
	dec.DisallowUnknownFields()
	if err = dec.Decode(&status); err != nil {
		return Status{}, errors.New("invalid guest job response")
	}
	var extra any
	if err = dec.Decode(&extra); err != io.EOF {
		return Status{}, errors.New("guest job returned multiple responses")
	}
	if status.State == "error" {
		return status, errors.New(status.Detail)
	}
	return status, nil
}

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.buffer.Len() {
		return 0, errors.New("guest response exceeds protocol limit")
	}
	return b.buffer.Write(p)
}
func (b *boundedOutput) Read(p []byte) (int, error) { return b.buffer.Read(p) }
func (c Client) Submit(ctx context.Context, req Request) (Status, error) {
	if len(req.Args) == 0 || !strings.HasPrefix(req.Dir, "/work/envctl/") || req.TimeoutSeconds < 1 {
		return Status{}, errors.New("invalid guest job request")
	}
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	if req.Secrets == nil {
		req.Secrets = []string{}
	}
	return c.call(ctx, "submit", req)
}
func (c Client) Poll(ctx context.Context, id string, cursor int64) (Status, error) {
	return c.call(ctx, "status", map[string]any{"id": id, "cursor": cursor})
}

// Reconcile completes a durable start intent using its original private request.
// It cannot create a missing request or restart a job that already began.
func (c Client) Reconcile(ctx context.Context, id string) (Status, error) {
	return c.call(ctx, "reconcile", map[string]string{"id": id})
}
func (c Client) Cancel(ctx context.Context, id string) (Status, error) {
	return c.call(ctx, "cancel", map[string]string{"id": id})
}
