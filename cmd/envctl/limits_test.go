package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRunShowReportsEffectiveNodeLimits(t *testing.T) {
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature, nodes: {qa: {limits: {max_attempts: 2, attempt_seconds: 7200}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("feature", "exercise limits", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rev := run.Current()
	rev.Attempts = append(rev.Attempts, workflow.Attempt{ID: "attempt_one", Node: "qa", State: "failed"})
	limits := nodeLimits(rev)
	if got := limits["qa"]; got.MaxAttempts != 2 || got.AttemptSeconds != 7200 || got.Attempts != 1 {
		t.Fatalf("qa limits: %+v", got)
	}
	if got := limits["code"]; got.MaxAttempts != c.Limits.MaxAttempts || got.AttemptSeconds != c.Limits.AttemptSeconds || got.Attempts != 0 {
		t.Fatalf("code did not inherit revision limits: %+v", got)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err = printLimits(cmd, rev); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "qa           1 of 2 attempts, 7200s per attempt") || !strings.HasPrefix(strings.TrimSpace(out.String()), "task") {
		t.Fatalf("unexpected limits output:\n%s", out.String())
	}
}
