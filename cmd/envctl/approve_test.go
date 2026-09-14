package main

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestApproveDefaultsToTheSingleWaitingResult(t *testing.T) {
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("demo", "objective", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var req daemon.ActionRequest
	if err = defaultApproval(&req, run); err == nil || !strings.Contains(err.Error(), "nothing") {
		t.Fatalf("no waiting attempt: %v", err)
	}
	result := &workflow.Result{Summary: "ready", Commits: map[string]string{"app": strings.Repeat("d", 40)}}
	v := run.Current()
	v.Attempts = []workflow.Attempt{{ID: "attempt_a", Node: "approved-change", State: "awaiting-approval", Result: result}}
	req = daemon.ActionRequest{}
	if err = defaultApproval(&req, run); err != nil || req.Attempt != "attempt_a" || req.WorkDigest != result.WorkDigest() {
		t.Fatalf("defaults not filled: %+v %v", req, err)
	}
	req = daemon.ActionRequest{WorkDigest: "pinned"}
	if err = defaultApproval(&req, run); err != nil || req.WorkDigest != "pinned" {
		t.Fatal("an explicit digest was replaced")
	}
	v.Attempts = append(v.Attempts, workflow.Attempt{ID: "attempt_b", Node: "other", State: "awaiting-approval", Result: result})
	req = daemon.ActionRequest{}
	if err = defaultApproval(&req, run); err == nil || !strings.Contains(err.Error(), "--attempt") {
		t.Fatalf("ambiguous approval accepted: %v", err)
	}
	req = daemon.ActionRequest{Attempt: "attempt_b"}
	if err = defaultApproval(&req, run); err != nil || req.Attempt != "attempt_b" {
		t.Fatalf("explicit attempt: %+v %v", req, err)
	}

	v.Attempts = []workflow.Attempt{{ID: "attempt_a", Node: "approved-change", State: "verifying", Result: result, Approval: &workflow.Approval{Actor: "dev", ResultDigest: result.WorkDigest()}}}
	req = daemon.ActionRequest{}
	if err = defaultApproval(&req, run); err == nil || !strings.Contains(err.Error(), "already approved and is publishing") {
		t.Fatalf("approved work reported as nothing waiting: %v", err)
	}
	v.Recovery = &workflow.Recovery{Phase: "publication", Detail: "destination base changed; approval must follow revalidated QA"}
	if err = defaultApproval(&req, run); err == nil || !strings.Contains(err.Error(), "publishing failed: destination base changed") || !strings.Contains(err.Error(), "envctl run rewind "+run.ID+" --to approved-change") {
		t.Fatalf("publication failure not explained: %v", err)
	}
}
