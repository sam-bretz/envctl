package localexec

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"testing"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// submitFixture records guest job submissions and reports them as running.
type submitFixture struct {
	vm.Provider
	submitted map[string]guestjob.Request
}

func (f *submitFixture) Exec(_ context.Context, _ string, c vm.Command) error {
	raw, err := io.ReadAll(c.Stdin)
	if err != nil {
		return err
	}
	var req guestjob.Request
	if err = json.Unmarshal(raw, &req); err != nil {
		return err
	}
	state := "missing"
	if c.Args[len(c.Args)-1] == "submit" {
		f.submitted[req.ID] = req
	}
	if _, ok := f.submitted[req.ID]; ok {
		state = "running"
	}
	return json.NewEncoder(c.Stdout).Encode(map[string]any{"id": req.ID, "state": state, "cursor": 0, "truncated": false})
}

func TestNodeAttemptSecondsBoundChecksWithoutTheirOwnTimeout(t *testing.T) {
	b, a := fixture(t)
	f := &submitFixture{submitted: map[string]guestjob.Request{}}
	b.Provider = f
	t.Setenv("ENVCTL_LIMITS_FIXTURE_UNSET", "")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_LIMITS_FIXTURE_UNSET"
	a.Revision.Config.Agents.Supervisor.Credential = "env:ENVCTL_LIMITS_FIXTURE_UNSET"
	code := a.Revision.Config.Workflow.Nodes["code"]
	code.Limits.AttemptSeconds = 4321
	code.Checks = []workflow.Check{{Name: "unit", Command: []string{"make", "test"}}, {Name: "slow", Command: []string{"make", "slow"}, TimeoutSeconds: 60}}
	a.Revision.Config.Workflow.Nodes["code"] = code
	if a.Revision.Config.Limits.AttemptSeconds == 4321 {
		t.Fatal("fixture cannot distinguish node and revision budgets")
	}
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "checks", Directories: map[string]string{"app": "/work/envctl/repos/app"}, Cursors: map[string]int64{}, Logs: map[string]string{}}
	poll := func(index int) guestjob.Request {
		t.Helper()
		r.CheckIndex = index
		if err := b.save(a, r); err != nil {
			t.Fatal(err)
		}
		if o, err := b.Poll(context.Background(), a); err != nil || o.State != "running" {
			t.Fatal("check was not dispatched", o, err)
		}
		req, ok := f.submitted[a.Attempt.ID+"_check_"+strconv.Itoa(index)]
		if !ok {
			t.Fatal("check request was not submitted")
		}
		return req
	}
	if req := poll(0); req.TimeoutSeconds != 4321 {
		t.Fatalf("check without its own timeout used %ds, not the node budget", req.TimeoutSeconds)
	}
	if req := poll(1); req.TimeoutSeconds != 60 {
		t.Fatalf("explicit check timeout was replaced: %ds", req.TimeoutSeconds)
	}
}
