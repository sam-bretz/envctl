package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// comparingModel is the dashboard fixture with its design stage branched into
// variations, every candidate finished and waiting for a choice.
func comparingModel(t *testing.T) (Model, string) {
	t.Helper()
	m := modelFixture(t)
	m.Width, m.Height = 140, 50
	r := &m.Runs[0]
	rev := r.Current()
	design := rev.Config.Workflow.Nodes["design"]
	design.Variations = 3
	rev.Config.Workflow.Nodes["design"] = design
	for _, node := range []string{"task", "plan", "design"} {
		rev.Checkpoints[node] = workflow.Checkpoint{ID: "cp_" + node, Node: node, Result: workflow.Result{Review: workflow.Review{Summary: "sound"}}}
	}
	rev.Attempts = []workflow.Attempt{{ID: "attempt_design", Node: "design", Number: 1, State: "checkpointed",
		Result: &workflow.Result{Summary: "webhooks", Review: workflow.Review{Accepted: true, Summary: "sound",
			Variations: []workflow.Variation{{Name: "polling", Rationale: "if the upstream has no webhooks"}, {Name: "streaming", Rationale: "at high volume"}}}}}}
	if err := r.Branch(r.CurrentRevision, "design", rev.Attempts[0].Result.Review.Variations, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := range r.Revisions {
		v := &r.Revisions[i]
		v.State = "active"
		for id, n := range v.Config.Workflow.Nodes {
			if n.Kind != "change" {
				v.Checkpoints[id] = workflow.Checkpoint{ID: "cp_" + id + v.ID, Node: id}
			}
		}
	}
	polling, err := r.FindVariation("polling")
	if err != nil {
		t.Fatal(err)
	}
	return m, polling.ID
}

func TestAWaitingComparisonIsAnnouncedInTheRunSummary(t *testing.T) {
	for _, name := range panels {
		m, _ := comparingModel(t)
		m = panel(t, m, name)
		if view := m.View().Content; !strings.Contains(view, "Comparing 3 variations of design · v to compare · C to choose") {
			t.Fatalf("a waiting choice is not visible on the %s tab:\n%s", name, view)
		}
	}
}

func TestChatShowsTheProposalsAndWhichVariationYouAreViewing(t *testing.T) {
	m, polling := comparingModel(t)
	m = panel(t, m, "Chat")
	m.Node = indexOf(orderOf(m), "design")
	chat := m.details()
	for _, want := range []string{"proposed alternatives, each built for comparison:", "polling — if the upstream has no webhooks", "streaming — at high volume", "Variation: as proposed"} {
		if !strings.Contains(chat, want) {
			t.Fatalf("missing %q:\n%s", want, chat)
		}
	}
	m.ViewedRevision = polling
	if chat = m.details(); !strings.Contains(chat, "Variation: polling — if the upstream has no webhooks (comparing") {
		t.Fatalf("viewing a variation does not say which:\n%s", chat)
	}
}

func TestAViewedVariationCanBeSteeredAndAskedButNotApproved(t *testing.T) {
	m, polling := comparingModel(t)
	m.ViewedRevision = polling
	m, _ = key(m, "?")
	for _, r := range "why polling?" {
		m, _ = key(m, string(r))
	}
	m, cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("asking a variation sent nothing")
	}
	cmd()
	if req := m.API.(*fakeAPI).request; req.Action != "ask" || req.Revision != polling {
		t.Fatalf("the question did not go to the variation being viewed: %+v", req)
	}
	// Approval of a variation is not something to do from under a comparison.
	m.API.(*fakeAPI).request.Action = ""
	msg := m.act(daemon.ActionRequest{Action: "approve"})()
	if am, ok := msg.(actionMsg); !ok || am.err == nil || m.API.(*fakeAPI).request.Action != "" {
		t.Fatal("approving a variation that is not current was not refused")
	}
}

func TestChoosingAVariationByNameSendsItsRevision(t *testing.T) {
	m, polling := comparingModel(t)
	m, _ = key(m, "C")
	if m.Mode != "choose" || !strings.Contains(m.View().Content, "never published") {
		t.Fatalf("C did not open the choice, with its consequence: mode %q", m.Mode)
	}
	for _, r := range "polling" {
		m, _ = key(m, string(r))
	}
	m, cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("choosing sent nothing")
	}
	cmd()
	if req := m.API.(*fakeAPI).request; req.Action != "choose" || req.Revision != polling {
		t.Fatalf("not sent as a choice of polling: %+v", req)
	}
}

func TestTheDecisionLogRecordsTheProposalsAndTheChoice(t *testing.T) {
	m, polling := comparingModel(t)
	r := &m.Runs[0]
	if err := r.Choose(polling, time.Now()); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, d := range decisions(r) {
		joined += d.Stage + " | " + d.Actor + ": " + d.Reason + "\n"
	}
	for _, want := range []string{
		"design | supervisor: proposed building polling: if the upstream has no webhooks",
		"design | supervisor: proposed building streaming: at high volume",
		"design | you: chose polling over as proposed, streaming; the others were retired and will not be published",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	// A variation is not a rewind, and must not be logged as one.
	if strings.Contains(joined, "rewound") {
		t.Fatalf("a variation was logged as a rewind:\n%s", joined)
	}
}

func indexOf(xs []string, x string) int {
	for i, v := range xs {
		if v == x {
			return i
		}
	}
	return -1
}

func TestAMessageIsNotSentToWhateverTheSelectionBecameWhileTyping(t *testing.T) {
	// Opening the composer pins the run, revision and stage it was opened
	// for. If a rewind lands while typing, the message must not go to the new
	// revision the person never saw.
	m := modelFixture(t)
	m, _ = key(m, "i")
	for _, r := range "use the existing client" {
		m, _ = key(m, string(r))
	}
	r := &m.Runs[0]
	if _, err := r.Rewind("task", "a different objective", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	m, cmd := key(m, "enter")
	if cmd != nil || !strings.Contains(m.Error, "changed") || m.API.(*fakeAPI).request.Action != "" {
		t.Fatalf("a message went to a revision that appeared while typing: err %q", m.Error)
	}

	// Moving to another stage while typing is refused the same way.
	m = modelFixture(t)
	m, _ = key(m, "?")
	m, _ = key(m, "x")
	m.Node++
	if m, cmd = key(m, "enter"); cmd != nil || !strings.Contains(m.Error, "changed") {
		t.Fatalf("a question went to a stage selected while typing: err %q", m.Error)
	}
}

func TestVOpensTheSideBySideComparison(t *testing.T) {
	m, _ := comparingModel(t)
	m, cmd := key(m, "v")
	if cmd == nil {
		t.Fatal("v fetched no comparison")
	}
	next, _ := m.Update(cmd())
	m = next.(Model)
	for _, want := range []string{"as proposed  [ready to choose]", "polling  [ready to choose]", "streaming  [ready to choose]", "C to choose one"} {
		if !strings.Contains(m.ArtifactText, want) {
			t.Fatalf("comparison missing %q:\n%s", want, m.ArtifactText)
		}
	}
}
