package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestPrintRunShowsCaptainsLogCounts(t *testing.T) {
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("demo", "objective", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if err = printRun(cmd, &globals{}, run); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "captain's log") {
		t.Fatal("captain's log line shown for a run with no tracker entries")
	}

	run.TrackerLog = []workflow.TrackerLogEntry{{Status: "pending"}, {Status: "pending"}, {Status: "failed"}}
	out.Reset()
	if err = printRun(cmd, &globals{}, run); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "captain's log: 2 pending, 1 failed") {
		t.Fatalf("captain's log counts not shown:\n%s", out.String())
	}
}
