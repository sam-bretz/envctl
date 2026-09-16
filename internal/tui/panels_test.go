package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// panel selects a detail view by name, so a test does not break when the
// tabs are reordered.
func panel(t *testing.T, m Model, name string) Model {
	t.Helper()
	i := slices.Index(panels, name)
	if i < 0 {
		t.Fatalf("no %q panel in %v", name, panels)
	}
	m.Panel = i
	return m
}

func TestTheTabsAreCutDownToWhatHelpsReviewTheWork(t *testing.T) {
	want := []string{"Chat", "Checkpoint", "Changes", "Tests", "Decision Log"}
	if !slices.Equal(panels, want) {
		t.Fatalf("tabs: %v, want %v", panels, want)
	}
}

func TestRemovingTheReadinessTabDidNotHideWhatBlocksARun(t *testing.T) {
	// This was on Readiness, two keypresses from where you land. A run that
	// cannot proceed has to say so on the first screen.
	m := panel(t, modelFixture(t), "Chat")
	m.Width, m.Height = 120, 40
	if !strings.Contains(m.View().Content, "harness.worker: not probed") {
		t.Fatalf("a readiness problem is not visible on Chat:\n%s", m.View().Content)
	}
	rev := m.Runs[0].Current()
	rev.Recovery = &workflow.Recovery{Phase: "publication", Detail: "GitHub rejected the push"}
	if !strings.Contains(m.details(), "GitHub rejected the push") {
		t.Fatalf("recovery is not visible on Chat:\n%s", m.details())
	}
}

func TestOnlyUnhealthyServicesSurfaceNowTheServicesTabIsGone(t *testing.T) {
	m := panel(t, modelFixture(t), "Chat")
	m.Width, m.Height = 120, 40
	rev := m.Runs[0].Current()
	rev.Runtime = workflow.RuntimeState{ID: "vm", State: "running", Ready: true, PreviewURL: "http://127.0.0.1:41234/", Services: []workflow.Service{
		{Name: "web", State: "running/healthy"}, {Name: "db", State: "exited"},
	}}
	details := m.details()
	if !strings.Contains(details, "db is exited") {
		t.Fatalf("a down service is hidden:\n%s", details)
	}
	if strings.Contains(details, "web is") {
		t.Fatalf("a healthy service is listed as a problem:\n%s", details)
	}
	// The preview URL was the Services tab's other job; the header keeps it.
	if !strings.Contains(m.View().Content, "Preview: http://127.0.0.1:41234/") {
		t.Fatal("the preview URL is no longer shown anywhere")
	}
}

func TestACheckpointSaysWhichStagesItRunsAfter(t *testing.T) {
	// Graph was removed; a stage's dependencies are the part worth keeping.
	m := panel(t, modelFixture(t), "Checkpoint")
	m.Node = slices.Index(orderOf(m), "plan")
	if !strings.Contains(m.details(), "Runs after: task") {
		t.Fatalf("dependencies not shown:\n%s", m.details())
	}
}

func TestTheComposerSaysItSteersTheWork(t *testing.T) {
	// A later "ask" mode must not be mistaken for this one.
	m := modelFixture(t)
	m.Width, m.Height = 120, 40
	if !strings.Contains(m.View().Content, "Steer ") {
		t.Fatalf("the idle prompt does not say it steers:\n%s", m.View().Content)
	}
	m, _ = key(m, "i")
	if !strings.Contains(m.View().Content, "changes the work") {
		t.Fatalf("the composer does not say it changes the work:\n%s", m.View().Content)
	}
}

func orderOf(m Model) []string {
	order, _ := m.Runs[0].Current().Config.Workflow.Order()
	return order
}

func TestTheDecisionLogExplainsWhatHappenedInOrder(t *testing.T) {
	m := modelFixture(t)
	r := &m.Runs[0]
	rev := r.Current()
	base := rev.CreatedAt
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	rev.DiscoveredRequirements = []workflow.Requirement{{Capability: "runtime.compose", Nodes: []string{"qa"}, Reason: "QA needs the app running"}}
	rev.Attempts = []workflow.Attempt{
		{ID: "a1", Node: "code", Number: 1, State: "failed", Error: correctionPrefix + "tests do not cover the error path\nmore detail", UpdatedAt: at(3)},
		{ID: "a2", Node: "code", Number: 2, State: "checkpointed", Result: &workflow.Result{Review: workflow.Review{Accepted: true, Summary: "covers every path now"}}, UpdatedAt: at(5)},
		{ID: "a3", Node: "approved-change", Number: 1, State: "checkpointed", Approval: &workflow.Approval{Actor: "local", At: at(7)}, UpdatedAt: at(6)},
		{ID: "a4", Node: "qa", Number: 1, State: "failed", Error: "guest job timed out", UpdatedAt: at(4)},
	}
	rev.Messages = []workflow.Message{{ID: "m1", Node: "code", Recipient: "worker", Body: "handle the empty case", CreatedAt: at(2)}}

	log := decisions(r)
	reasons := make([]string, len(log))
	for i, d := range log {
		reasons[i] = d.Actor + ": " + d.Reason
	}
	joined := strings.Join(reasons, "\n")

	for _, want := range []string{
		"coordinator: run started",
		"worker: found that qa needs runtime.compose: QA needs the app running",
		"you: told the worker: handle the empty case",
		// A rejection is the supervisor's, with its correction, not a failure.
		"supervisor: rejected attempt 1: tests do not cover the error path",
		"coordinator: attempt 1 failed: guest job timed out",
		"supervisor: accepted attempt 2: covers every path now",
		"you: approved attempt 1",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "more detail") {
		t.Fatalf("an entry is not kept to one line:\n%s", joined)
	}
	for i := 1; i < len(log); i++ {
		if log[i].At.Before(log[i-1].At) {
			t.Fatalf("out of order at %d:\n%s", i, joined)
		}
	}
}

func TestADecisionLogSaysWhatARewindRerunAndWhatChanged(t *testing.T) {
	log := func(r *workflow.Run) string {
		joined := ""
		for _, d := range decisions(r) {
			joined += d.Actor + ": " + d.Reason + "\n"
		}
		return joined
	}

	// Keeping the objective keeps task's checkpoint, so a rewind to plan
	// reruns from plan.
	m := modelFixture(t)
	r := &m.Runs[0]
	r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_task", Node: "task", Result: workflow.Result{Summary: "task"}}
	if _, err := r.Rewind("plan", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := log(r); !strings.Contains(got, "you: rewound, rerunning from plan\n") {
		t.Fatalf("rewind not explained:\n%s", got)
	}

	// Changing the objective reruns every stage whatever was picked, so the
	// log must not claim it reran from plan.
	m = modelFixture(t)
	r = &m.Runs[0]
	r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_task", Node: "task", Result: workflow.Result{Summary: "task"}}
	if _, err := r.Rewind("plan", "a sharper objective", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := log(r); !strings.Contains(got, "you: rewound, rerunning from task, changing the objective") {
		t.Fatalf("objective change not explained:\n%s", got)
	}
}
