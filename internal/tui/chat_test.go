package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestChatReadsAsOneConversationInTheOrderItHappened(t *testing.T) {
	m := panel(t, modelFixture(t), "Chat")
	m.Width, m.Height = 160, 60
	rev := m.Runs[0].Current()
	node := m.nodeID()
	base := time.Date(2026, 9, 16, 9, 0, 0, 0, time.Local)
	at := func(min int) time.Time { return base.Add(time.Duration(min) * time.Minute) }
	rev.Attempts = []workflow.Attempt{{ID: "attempt_1", Node: node, Number: 1, State: "awaiting-approval", StartedAt: at(0), UpdatedAt: at(3),
		Result: &workflow.Result{Summary: "Wrote the exporter", Review: workflow.Review{Accepted: true, Summary: "covers every path"}}}}
	rev.Messages = []workflow.Message{{ID: "m1", Node: node, Recipient: "worker", Body: "handle empty files", CreatedAt: at(1)}}
	rev.Questions = []workflow.Question{
		{ID: "q1", Node: node, Text: "is there a PR open for this?", State: workflow.QuestionAnswered, Answer: "Not yet; approved-change opens it.", CreatedAt: at(4), AnsweredAt: at(5)},
		{ID: "q2", Node: node, Text: "which parser?", State: workflow.QuestionAnswering, CreatedAt: at(6)},
		{ID: "q3", Node: node, Text: "why?", State: workflow.QuestionFailed, Detail: "the stage's VM is no longer running", CreatedAt: at(7), AnsweredAt: at(7)},
	}

	chat := m.details()
	order := []string{
		"coordinator: attempt 1 started",
		"you → worker (steer): handle empty files",
		"worker: Wrote the exporter",
		"supervisor: accepted: covers every path",
		"waiting for your approval",
		"you → supervisor (ask): is there a PR open for this?",
		"supervisor: Not yet; approved-change opens it.",
		"you → supervisor (ask): which parser?",
		"supervisor: thinking",
		"you → supervisor (ask): why?",
		"supervisor: could not answer: the stage's VM is no longer running",
	}
	last := -1
	for _, want := range order {
		i := strings.Index(chat, want)
		if i < 0 {
			t.Fatalf("missing %q:\n%s", want, chat)
		}
		if i < last {
			t.Fatalf("%q is out of order:\n%s", want, chat)
		}
		last = i
	}
}

func TestAnApprovalShowsWhoApprovedAndWhenWithTheStage(t *testing.T) {
	m := panel(t, modelFixture(t), "Chat")
	rev := m.Runs[0].Current()
	node := m.nodeID()
	approved := time.Date(2026, 9, 16, 14, 30, 0, 0, time.Local)
	rev.Attempts = []workflow.Attempt{{ID: "attempt_1", Node: node, Number: 1, State: "checkpointed",
		Approval: &workflow.Approval{Actor: "sam", At: approved}}}
	if chat := m.details(); !strings.Contains(chat, "sam: approved attempt 1 at 2026-09-16 14:30") {
		t.Fatalf("approval actor and time not shown with the stage:\n%s", chat)
	}
}

func TestAStageCarriedOverByARewindStillShowsItsResult(t *testing.T) {
	// A rewind copies earlier checkpoints into the new revision without the
	// attempts that produced them, so the thread must read the checkpoint too.
	m := panel(t, modelFixture(t), "Chat")
	rev := m.Runs[0].Current()
	node := m.nodeID()
	rev.Attempts = nil
	rev.Checkpoints[node] = workflow.Checkpoint{ID: "cp_old", Node: node, Attempt: "attempt_in_an_older_revision",
		Result: workflow.Result{Summary: "the original design", Review: workflow.Review{Summary: "sound"},
			Artifacts: []workflow.Artifact{{Name: "design", MediaType: "text/markdown", Size: 42}}}}
	chat := m.details()
	for _, want := range []string{"carried over from an earlier revision", "worker: the original design", "supervisor: accepted: sound"} {
		if !strings.Contains(chat, want) {
			t.Fatalf("an inherited result is invisible (missing %q):\n%s", want, chat)
		}
	}
	// The artifact list replaces the Checkpoint tab, so , . and o still have
	// something to select.
	if !strings.Contains(chat, "> design · text/markdown · 42 bytes") {
		t.Fatalf("the stage's artifacts are not listed in Chat:\n%s", chat)
	}
}

func TestQuestionMarkAsksTheSupervisorWithoutSteering(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 120, 40
	m, _ = key(m, "?")
	if m.Mode != "ask" || !strings.Contains(m.View().Content, "does not change the work") {
		t.Fatalf("? did not open the ask composer: mode %q", m.Mode)
	}
	for _, r := range "is there a PR?" {
		m, _ = key(m, string(r))
	}
	m, cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("asking sent nothing")
	}
	cmd()
	req := m.API.(*fakeAPI).request
	if req.Action != "ask" || req.Node != m.nodeID() || req.Message != "is there a PR?" || req.Recipient != "" {
		t.Fatalf("not sent as a question to this stage's supervisor: %+v", req)
	}
}

func TestTabSwitchesAMessageBetweenSteeringAndAsking(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 120, 40
	m, _ = key(m, "i")
	if m.Mode != "chat" || !strings.Contains(m.View().Content, "changes the work") {
		t.Fatalf("i did not open the steer composer: mode %q", m.Mode)
	}
	m, _ = key(m, "tab")
	if m.Mode != "ask" {
		t.Fatalf("tab did not switch to asking: %q", m.Mode)
	}
	m, _ = key(m, "tab")
	if m.Mode != "chat" {
		t.Fatalf("tab did not switch back to steering: %q", m.Mode)
	}
}
