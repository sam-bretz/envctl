package workflow

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBoundActivityIsSingleLinePrintableAndNewest(t *testing.T) {
	var lines []string
	for i := 0; i < ProgressActivityLines+5; i++ {
		lines = append(lines, "line")
	}
	lines = append(lines, "  ", "multi\nline\x1b[2J\ttab", strings.Repeat("é", ProgressLineBytes))
	got := BoundActivity(lines)
	if len(got) != ProgressActivityLines {
		t.Fatal("activity not bounded", len(got))
	}
	if got[len(got)-2] != "multi line[2J tab" {
		t.Fatalf("control characters or newlines kept: %q", got[len(got)-2])
	}
	last := got[len(got)-1]
	if len(last) > ProgressLineBytes || !utf8.ValidString(last) || !strings.HasSuffix(last, "…") {
		t.Fatal("long line not truncated on a rune boundary")
	}
}

func TestMessageTargetsAndStatus(t *testing.T) {
	worker := Message{ID: "m1", Node: "code", Recipient: "worker"}
	anyStage := Message{ID: "m2", Recipient: "supervisor"}
	if !worker.Targets("code", "worker") || worker.Targets("qa", "worker") || !worker.Targets("code", "supervisor") {
		t.Fatal("node-scoped worker message targeting")
	}
	if anyStage.Targets("code", "worker") || !anyStage.Targets("qa", "supervisor") {
		t.Fatal("supervisor message delivered to worker")
	}
	v := &Revision{Messages: []Message{worker, anyStage}}
	if got := v.MessageStatus(worker); got != "queued for the next code attempt" {
		t.Fatal(got)
	}
	v.Attempts = []Attempt{{ID: "a1", Node: "code", Number: 2, State: "running"}}
	if got := v.MessageStatus(worker); got != "pending live delivery to code attempt 2" {
		t.Fatal(got)
	}
	v.Attempts[0].Steering = []Delivery{{Message: "m1", Role: "worker", Generation: 3, At: time.Now()}, {Message: "m1", Role: "supervisor", At: time.Now()}}
	if got := v.MessageStatus(worker); got != "delivered live to code worker attempt 2 (resume 3); included when code supervisor attempt 2 started" {
		t.Fatal(got)
	}
	if !SameProgress(&Progress{Phase: "worker", Activity: []string{"a"}, UpdatedAt: time.Now()}, &Progress{Phase: "worker", Activity: []string{"a"}}) || SameProgress(nil, &Progress{}) {
		t.Fatal("progress comparison must ignore only the observation time")
	}
}
