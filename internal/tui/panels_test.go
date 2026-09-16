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
	want := []string{"Chat", "Changes", "Tests", "Decision Log"}
	if !slices.Equal(panels, want) {
		t.Fatalf("tabs: %v, want %v", panels, want)
	}
}

func TestWhatBlocksARunIsInTheRunSummaryWhateverTabIsOpen(t *testing.T) {
	// Readiness and recovery used to be on a tab of their own, two keypresses
	// from where you land. They belong in the summary, visible from any tab.
	for _, name := range panels {
		m := panel(t, modelFixture(t), name)
		m.Width, m.Height = 120, 40
		rev := m.Runs[0].Current()
		rev.Recovery = &workflow.Recovery{Phase: "publication", Detail: "GitHub rejected the push"}
		view := m.View().Content
		if !strings.Contains(view, "harness.worker: not probed") || !strings.Contains(view, "GitHub rejected the push") {
			t.Fatalf("a blocker is hidden on the %s tab:\n%s", name, view)
		}
	}
}

func TestServiceStatesShowWhenThereIsAReasonToLook(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 120, 40
	rev := m.Runs[0].Current()
	rev.Runtime = workflow.RuntimeState{ID: "vm", State: "running", Ready: true, PreviewURL: "http://127.0.0.1:41234/", Services: []workflow.Service{
		{Name: "web", State: "running/healthy"}, {Name: "db", State: "exited"},
	}}
	// A service that is down is shown, because it is often why a stage failed.
	view := m.View().Content
	if !strings.Contains(view, "db exited") {
		t.Fatalf("a down service is hidden:\n%s", view)
	}
	if !strings.Contains(view, "Preview: http://127.0.0.1:41234/") {
		t.Fatal("the preview URL is no longer shown anywhere")
	}
	// All healthy and no preview configured: nothing worth a line.
	rev.Runtime.Services[1].State = "running/healthy"
	if view = m.View().Content; strings.Contains(view, "web running/healthy") {
		t.Fatalf("healthy services shown with no preview to look at:\n%s", view)
	}
	// With a preview configured, the services are what you came to look at.
	rev.Config.Preview = &workflow.Preview{Service: "web", Port: 8080}
	if view = m.View().Content; !strings.Contains(view, "web running/healthy") {
		t.Fatalf("services hidden although a preview is configured:\n%s", view)
	}
}

func TestChatSaysWhichStagesTheSelectedStageRunsAfter(t *testing.T) {
	// Graph was removed; a stage's dependencies are the part worth keeping.
	m := panel(t, modelFixture(t), "Chat")
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

func TestTheDecisionLogIncludesQuestionsAndTheCoordinatorsStops(t *testing.T) {
	m := modelFixture(t)
	r := &m.Runs[0]
	rev := r.Current()
	base := rev.CreatedAt
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	rev.Questions = []workflow.Question{
		{ID: "q1", Node: "code", Asker: "local", Text: "is there a PR open?", State: workflow.QuestionAnswered, Answer: "not yet", CreatedAt: at(1)},
		{ID: "q2", Node: "code", Text: "why?", State: workflow.QuestionFailed, Detail: "VM released", CreatedAt: at(2)},
	}
	rev.Notes = []workflow.Note{
		{At: at(3), Kind: workflow.NoteStallNudge, Node: "code", Detail: "nudged the worker after a silence (nudge 1 of 2)"},
		{At: at(4), Kind: workflow.NoteAttemptBudget, Node: "qa", Detail: "stage qa exhausted its configured 3 attempts"},
		{At: at(5), Kind: workflow.NoteTokenCeiling, Detail: "run counted 20.0M of its 20.0M token ceiling"},
		{At: at(6), Kind: workflow.NotePublishFailed, Detail: "base branch main moved"},
		{At: at(7), Kind: workflow.NotePublished, Node: "approved-change", Detail: "app: https://github.com/o/r/pull/7"},
	}
	joined := ""
	for _, d := range decisions(r) {
		joined += d.Stage + " | " + d.Actor + ": " + d.Reason + "\n"
	}
	for _, want := range []string{
		"code | you: asked the supervisor: is there a PR open? [answered: not yet]",
		// A question with no recorded asker, as from MCP, is still yours.
		"code | you: asked the supervisor: why? [not answered: VM released]",
		"code | coordinator: nudged the worker after a silence (nudge 1 of 2)",
		"qa | coordinator: stopped retrying: stage qa exhausted its configured 3 attempts",
		"| coordinator: stopped at the token ceiling: run counted 20.0M of its 20.0M token ceiling",
		"| coordinator: could not publish: base branch main moved",
		"approved-change | coordinator: opened the pull request: app: https://github.com/o/r/pull/7",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
}

func TestTheDiffKeyOpensChangesWhereverTheTabsAre(t *testing.T) {
	m := modelFixture(t)
	node := m.nodeID()
	m.Runs[0].Current().Checkpoints[node] = workflow.Checkpoint{ID: "cp", Node: node}
	m, _ = key(m, "d")
	if got := panels[m.Panel]; got != "Changes" {
		t.Fatalf("d opened %s, not Changes", got)
	}
}
