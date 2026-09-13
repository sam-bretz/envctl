package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRunPRPrintsAndOpens(t *testing.T) {
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("demo", "objective", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err = printPullRequests(&out, false, false, run, nil); err == nil || !strings.Contains(err.Error(), "no pull request yet") {
		t.Fatalf("missing PR not reported: %v", err)
	}
	out.Reset()
	if err = printPullRequests(&out, true, false, run, nil); err != nil || strings.TrimSpace(out.String()) != "[]" {
		t.Fatalf("json without PRs: %q %v", out.String(), err)
	}
	run.Current().Checkpoints["approved-change"] = workflow.Checkpoint{Result: workflow.Result{PRs: map[string]string{"app": "https://github.com/you/app/pull/7"}}}
	var opened []string
	out.Reset()
	if err = printPullRequests(&out, false, true, run, func(u string) error { opened = append(opened, u); return nil }); err != nil {
		t.Fatal(err)
	}
	if out.String() != "https://github.com/you/app/pull/7  app\n" || len(opened) != 1 {
		t.Fatalf("output %q opened %v", out.String(), opened)
	}
}
