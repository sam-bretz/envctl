package engine

import (
	"context"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// answeringBackend is the fixture backend plus the ability to answer. It
// answers on the second call, so a test sees "answering" before "answered".
type answeringBackend struct {
	*fixtureBackend
	calls map[string]int
	fail  bool
}

func (b *answeringBackend) Answer(_ context.Context, a Assignment, q workflow.Question) (QuestionObservation, error) {
	b.calls[q.ID]++
	if b.fail {
		return QuestionObservation{State: workflow.QuestionFailed, Detail: "harness exited"}, nil
	}
	if b.calls[q.ID] < 2 {
		return QuestionObservation{State: "running"}, nil
	}
	return QuestionObservation{State: workflow.QuestionAnswered, Answer: "PR #7 is open for " + a.Attempt.Node, Usage: &workflow.Usage{Input: 30, Output: 5}}, nil
}

func answering(h *harness) *answeringBackend {
	b := &answeringBackend{fixtureBackend: h.backend, calls: map[string]int{}}
	h.engine.Backend = b
	return b
}

func (h *harness) ask(node, text string) string {
	h.t.Helper()
	var id string
	h.mutate(func(r *workflow.Run) error {
		var err error
		id, err = r.Ask(h.rev, node, text, "you", h.now)
		return err
	})
	return id
}

func (h *harness) reconcileQuestion(id string) {
	h.t.Helper()
	if err := h.engine.reconcileQuestion(context.Background(), h.id, h.rev, id); err != nil {
		h.t.Fatal(err)
	}
}

func waitingForApproval(v *workflow.Revision) bool {
	for _, a := range v.Attempts {
		if a.State == "awaiting-approval" {
			return true
		}
	}
	return false
}

func TestAQuestionIsAnsweredWhetherTheStageIsRunningWaitingOrAccepted(t *testing.T) {
	h := setup(t)
	h.until(waitingForApproval)
	v := h.run().Current()
	waiting := ""
	for _, a := range v.Attempts {
		if a.State == "awaiting-approval" {
			waiting = a.Node
		}
	}

	// Running: a job that stays running.
	running := setup(t)
	running.backend.startRunning = true
	running.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.State == "running" {
				return true
			}
		}
		return false
	})
	runningNode := ""
	for _, a := range running.run().Current().Attempts {
		if a.State == "running" {
			runningNode = a.Node
		}
	}

	for _, c := range []struct {
		name string
		h    *harness
		node string
	}{
		{"running", running, runningNode},
		{"waiting for approval", h, waiting},
		{"accepted", h, "task"},
	} {
		t.Run(c.name, func(t *testing.T) {
			answering(c.h)
			before := *workflow.Clone(c.h.run().Current())
			id := c.h.ask(c.node, "is there a PR open for this?")

			c.h.reconcileQuestion(id)
			if q := c.h.run().Current().Question(id); q.State != workflow.QuestionAnswering {
				t.Fatalf("not marked answering: %+v", q)
			}
			c.h.reconcileQuestion(id)
			q := c.h.run().Current().Question(id)
			if q.State != workflow.QuestionAnswered || q.Answer == "" || q.Usage == nil || q.AnsweredAt.IsZero() {
				t.Fatalf("not answered: %+v", q)
			}

			// The work is exactly as it was: same attempts, states, results
			// and approvals.
			after := c.h.run().Current()
			if workflow.Digest(before.Attempts) != workflow.Digest(after.Attempts) || workflow.Digest(before.Checkpoints) != workflow.Digest(after.Checkpoints) {
				t.Fatal("answering a question changed the work")
			}
		})
	}
}

func TestAnAnswerIsRecordedOnceAndNeverAskedForAgain(t *testing.T) {
	h := setup(t)
	h.until(waitingForApproval)
	b := answering(h)
	id := h.ask("task", "why?")
	h.reconcileQuestion(id)
	h.reconcileQuestion(id)
	calls := b.calls[id]
	h.reconcileQuestion(id)
	h.reconcileQuestion(id)
	if b.calls[id] != calls {
		t.Fatalf("a finished question went back to the harness: %d calls, then %d", calls, b.calls[id])
	}
}

func TestAQuestionFailsClearlyWhenItCannotBeAnswered(t *testing.T) {
	t.Run("the harness failed", func(t *testing.T) {
		h := setup(t)
		h.until(waitingForApproval)
		answering(h).fail = true
		id := h.ask("task", "why?")
		h.reconcileQuestion(id)
		if q := h.run().Current().Question(id); q.State != workflow.QuestionFailed || q.Detail == "" {
			t.Fatalf("a failed answer was not reported: %+v", q)
		}
	})
	t.Run("the VM was released", func(t *testing.T) {
		h := setup(t)
		h.until(waitingForApproval)
		b := answering(h)
		id := h.ask("task", "why?")
		h.mutate(func(r *workflow.Run) error { r.Current().Runtime.Ready = false; return nil })
		h.reconcileQuestion(id)
		q := h.run().Current().Question(id)
		if q.State != workflow.QuestionFailed || b.calls[id] != 0 {
			t.Fatalf("answered without a VM to read the work from: %+v, %d calls", q, b.calls[id])
		}
	})
	t.Run("the backend cannot answer", func(t *testing.T) {
		h := setup(t)
		h.until(waitingForApproval)
		id := h.ask("task", "why?")
		h.reconcileQuestion(id)
		if q := h.run().Current().Question(id); q.State != workflow.QuestionFailed {
			t.Fatalf("a backend without answers left the question open: %+v", q)
		}
	})
	t.Run("the ceiling was reached before it started", func(t *testing.T) {
		h := setup(t)
		h.until(waitingForApproval)
		b := answering(h)
		id := h.ask("task", "why?")
		h.mutate(func(r *workflow.Run) error {
			r.Current().Config.Limits.RunTokens = 1
			r.Current().Attempts[0].Usage = &workflow.Usage{Input: 1000}
			return nil
		})
		h.reconcileQuestion(id)
		if q := h.run().Current().Question(id); q.State != workflow.QuestionFailed || b.calls[id] != 0 {
			t.Fatalf("started an answer over the ceiling: %+v", q)
		}
	})
}
