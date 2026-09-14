package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestApproveKeyNeverApprovesAnUnviewedStage(t *testing.T) {
	m := modelFixture(t)
	api := m.API.(*fakeAPI)
	v := m.Runs[0].Current()
	result := &workflow.Result{Summary: "change ready", Commits: map[string]string{"app": strings.Repeat("c", 40)}}
	v.Attempts = append(v.Attempts, workflow.Attempt{ID: "attempt_change", Node: "approved-change", State: "awaiting-approval", Number: 1, Result: result, StartedAt: time.Now()})

	// On Task, nothing is approved; the selection moves to the waiting stage.
	m, cmd := key(m, "a")
	if cmd != nil || api.request.Action != "" {
		t.Fatal("approved a stage the reviewer was not viewing")
	}
	if m.nodeID() != "approved-change" || !strings.Contains(m.Notice, "press a again") {
		t.Fatalf("did not move to the waiting stage: node %s notice %q", m.nodeID(), m.Notice)
	}
	// A second press on that stage approves the exact result.
	m, cmd = key(m, "a")
	if cmd == nil {
		t.Fatal("second press did not approve")
	}
	cmd()
	if api.request.Action != "approve" || api.request.Attempt != "attempt_change" || api.request.WorkDigest != result.WorkDigest() {
		t.Fatalf("approval request %+v", api.request)
	}

	// Approved work that failed to publish says why and how to recover.
	v = m.Runs[0].Current()
	v.Attempts[len(v.Attempts)-1].State = "verifying"
	v.Attempts[len(v.Attempts)-1].Approval = &workflow.Approval{Actor: "local", ResultDigest: result.WorkDigest()}
	v.Recovery = &workflow.Recovery{Phase: "publication", Detail: "destination base changed; approval must follow revalidated QA"}
	m, cmd = key(m, "a")
	if cmd != nil || !strings.Contains(m.Error, "already approved, but publishing failed: destination base changed") || !strings.Contains(m.Error, "press r") {
		t.Fatalf("no explanation for approved work that cannot publish: %q", m.Error)
	}

	// With nothing waiting, the key explains itself.
	m.Runs[0].Current().Attempts = nil
	m, _ = key(m, "a")
	if !strings.Contains(m.Error, "nothing in this run is awaiting approval") {
		t.Fatalf("no feedback when nothing is waiting: %q", m.Error)
	}
}
