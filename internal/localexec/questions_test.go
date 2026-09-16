package localexec

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func questionAssignment(t *testing.T) (engine.Assignment, *attemptRecord, workflow.Question) {
	t.Helper()
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature, nodes: {code: {agents: {supervisor: {model: claude-opus-5}}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	rev := workflow.Revision{ID: "rev_1", Objective: "Add CSV export", Config: c,
		Checkpoints: map[string]workflow.Checkpoint{},
		Messages:    []workflow.Message{{Node: "code", Recipient: "worker", Body: "handle empty files"}},
	}
	attempt := workflow.Attempt{ID: "attempt_1", Node: "code", Number: 2, State: "awaiting-approval", Result: &workflow.Result{
		Summary: "Wrote the exporter", Review: workflow.Review{Summary: "Covers the empty-file case"},
		Checks: []workflow.CheckResult{{Name: "unit", Passed: true}, {Name: "lint", Passed: false, ExitCode: 1}},
	}}
	q := workflow.Question{ID: "question_1", Node: "code", Attempt: "attempt_1", Text: "is there a PR open for this?"}
	record := &attemptRecord{Worker: agent.Invocation{Directory: "/work/envctl/repos/app"}, SupervisorSession: "01a08f59-7073-7a83-b959-867ca896ce48"}
	return engine.Assignment{Revision: rev, Attempt: attempt}, record, q
}

func TestAnAnswerComesFromTheStagesOwnSupervisorAndCannotWrite(t *testing.T) {
	a, record, q := questionAssignment(t)
	claude := workflow.Harness{Kind: "claude"}
	i := answerInvocation(a, record, q, "question_1_answer", claude)
	if !i.ReadOnly || i.Role != "supervisor" || i.Directory != "/work/envctl/repos/app" {
		t.Fatalf("not a read-only supervisor invocation in the worktree: %+v", i)
	}
	if i.Model != "claude-opus-5" {
		t.Fatalf("answered by %q, not the stage's supervisor model", i.Model)
	}
	// Claude branches the supervisor's session, so the answer is grounded in
	// what it reviewed without touching the transcript it resumes later.
	if i.Session != record.SupervisorSession || !i.Fork {
		t.Fatalf("claude did not fork the supervisor's session: %+v", i)
	}
	// Codex cannot branch a session, so it must start fresh rather than
	// append to one.
	codex := answerInvocation(a, record, q, "question_1_answer", workflow.Harness{Kind: "codex"})
	if codex.Session != "" || codex.Fork {
		t.Fatalf("codex would continue the supervisor's session: %+v", codex)
	}
	// With no supervisor session yet, as while the worker is still running,
	// the answer starts fresh from the facts.
	record.SupervisorSession = ""
	if fresh := answerInvocation(a, record, q, "question_1_answer", claude); fresh.Session != "" || fresh.Fork {
		t.Fatalf("resumed a session that does not exist: %+v", fresh)
	}
}

func TestTheAnswerIsGroundedInTheWorkNotTheModelsMemory(t *testing.T) {
	a, record, q := questionAssignment(t)
	prompt := questionPrompt(a, record, q)
	for _, want := range []string{
		"Add CSV export", "Attempt 2 is awaiting-approval",
		"Wrote the exporter", "Covers the empty-file case",
		"unit passed", "lint failed (exit 1)",
		"to the worker: handle empty files",
		// The example the issue is built around: this must be answerable.
		"No pull request has been opened for this run yet.",
		"is there a PR open for this?",
		"cannot change anything",
		"say so plainly instead of guessing",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt is missing %q:\n%s", want, prompt)
		}
	}

	// Once a pull request exists, the prompt names it instead.
	a.Revision.Checkpoints["approved-change"] = workflow.Checkpoint{Result: workflow.Result{PRs: map[string]string{"app": "https://github.com/o/r/pull/7"}}}
	prompt = questionPrompt(a, record, q)
	if !strings.Contains(prompt, "app: https://github.com/o/r/pull/7") || strings.Contains(prompt, "No pull request") {
		t.Fatalf("the published pull request is not in the prompt:\n%s", prompt)
	}

	// A follow-up sees what was already answered, which is what makes this a
	// conversation rather than a series of unrelated questions.
	a.Revision.Questions = []workflow.Question{{ID: "question_0", Node: "code", State: workflow.QuestionAnswered, Text: "which parser?", Answer: "encoding/csv"}, q}
	if prompt = questionPrompt(a, record, q); !strings.Contains(prompt, "Earlier question: which parser?\nYour answer: encoding/csv") {
		t.Fatalf("earlier answers are not carried into a follow-up:\n%s", prompt)
	}
}

func TestEachStallNudgeIsReportedWithWhenItWasSent(t *testing.T) {
	a, _, _ := questionAssignment(t)
	first, second := time.Date(2026, 9, 16, 10, 0, 0, 0, time.UTC), time.Date(2026, 9, 16, 10, 12, 0, 0, time.UTC)
	r := &attemptRecord{Stall: map[string]*stallState{
		"worker": {Nudges: []int{1, 2}, NudgedAt: []time.Time{first, second}},
	}}
	notes := r.notes(a)
	if len(notes) != 2 || !notes[0].At.Equal(first) || !notes[1].At.Equal(second) {
		t.Fatalf("nudges not reported with their times: %+v", notes)
	}
	if notes[1].Kind != workflow.NoteStallNudge || notes[1].Node != "code" || !strings.Contains(notes[1].Detail, "worker") || !strings.Contains(notes[1].Detail, "nudge 2 of") {
		t.Fatalf("nudge note does not say which role or which nudge: %+v", notes[1])
	}
	if (&attemptRecord{}).notes(a) != nil {
		t.Fatal("an attempt that was never nudged reported notes")
	}
}
