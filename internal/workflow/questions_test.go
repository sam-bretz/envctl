package workflow

import (
	"strings"
	"testing"
	"time"
)

func questionRun(t *testing.T) *Run {
	t.Helper()
	c, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRun("demo", "ship it", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAQuestionGoesToTheLatestAttemptOfItsStage(t *testing.T) {
	r := questionRun(t)
	rev := r.Current()
	rev.Attempts = []Attempt{{ID: "a1", Node: "code", Number: 1, State: "failed"}, {ID: "a2", Node: "code", Number: 2, State: "running"}}
	id, err := r.Ask(r.CurrentRevision, "code", "  is there a PR open for this?  ", "sam", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	q := rev.Question(id)
	if q == nil || q.Attempt != "a2" || q.State != QuestionPending || q.Text != "is there a PR open for this?" {
		t.Fatalf("question not recorded against the latest attempt: %+v", q)
	}
}

func TestAQuestionIsRefusedWhenItCannotBeAnswered(t *testing.T) {
	r := questionRun(t)
	r.Current().Attempts = []Attempt{{ID: "a1", Node: "code", Number: 1, State: "running"}}
	cases := map[string]func() error{
		"an unknown stage":      func() error { _, err := r.Ask(r.CurrentRevision, "nope", "why?", "", time.Now()); return err },
		"a stage not yet begun": func() error { _, err := r.Ask(r.CurrentRevision, "qa", "why?", "", time.Now()); return err },
		"an empty question":     func() error { _, err := r.Ask(r.CurrentRevision, "code", "   ", "", time.Now()); return err },
		"an old revision":       func() error { _, err := r.Ask("rev_old", "code", "why?", "", time.Now()); return err },
		"an oversized question": func() error {
			_, err := r.Ask(r.CurrentRevision, "code", strings.Repeat("x", MaxQuestionLength+1), "", time.Now())
			return err
		},
	}
	for name, ask := range cases {
		if err := ask(); err == nil {
			t.Fatalf("accepted %s", name)
		}
	}
	if len(r.Current().Questions) != 0 {
		t.Fatalf("a refused question was recorded: %+v", r.Current().Questions)
	}
}

func TestAQuestionIsRefusedOnceTheRunIsAtItsTokenCeiling(t *testing.T) {
	r := questionRun(t)
	rev := r.Current()
	rev.Config.Limits.RunTokens = 1000
	rev.Attempts = []Attempt{{ID: "a1", Node: "code", Number: 1, State: "running", Usage: &Usage{Input: 5000}}}
	_, err := r.Ask(r.CurrentRevision, "code", "why?", "", time.Now())
	if err == nil || !strings.Contains(err.Error(), "ceiling") {
		t.Fatalf("a question was accepted at the token ceiling: %v", err)
	}
}

func TestAnsweringAQuestionCountsTowardTheRunsTokens(t *testing.T) {
	r := questionRun(t)
	rev := r.Current()
	rev.Attempts = []Attempt{{ID: "a1", Node: "code", Number: 1, State: "running", Usage: &Usage{Input: 100}}}
	before := r.Usage().Tokens()
	rev.Questions = []Question{{ID: "q1", Node: "code", Attempt: "a1", State: QuestionAnswered, Usage: &Usage{Input: 40, Output: 10}}}
	if got := r.Usage().Tokens() - before; got != 50 {
		t.Fatalf("an answer added %d tokens to the run, want 50", got)
	}
}

func TestQuestionsLeaveTheWorkAndItsApprovalUnchanged(t *testing.T) {
	// Questions live beside the attempt, never inside its result, so the work
	// digest an approval binds to cannot move when one is asked or answered.
	r := questionRun(t)
	rev := r.Current()
	result := &Result{Summary: "built it", Commits: map[string]string{"app": strings.Repeat("a", 40)}}
	rev.Attempts = []Attempt{{ID: "a1", Node: "code", Number: 1, State: "awaiting-approval", Result: result}}
	bound := result.WorkDigest()
	if _, err := r.Ask(r.CurrentRevision, "code", "why this design?", "", time.Now()); err != nil {
		t.Fatal(err)
	}
	rev.Questions[0].State, rev.Questions[0].Answer = QuestionAnswered, "because"
	a := rev.Attempt("a1")
	if a.State != "awaiting-approval" || a.Result.WorkDigest() != bound {
		t.Fatalf("a question changed the attempt: state %s", a.State)
	}
}
