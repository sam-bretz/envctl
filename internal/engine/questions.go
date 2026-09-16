package engine

import (
	"context"
	"errors"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// QuestionBackend answers a person's question about a stage. It is optional,
// like PreviewBackend: a backend without it reports questions as unanswerable
// rather than blocking the run.
type QuestionBackend interface {
	// Answer is idempotent. The first call starts a read-only invocation for
	// the question; later calls report its progress, then its answer.
	Answer(context.Context, Assignment, workflow.Question) (QuestionObservation, error)
}

// QuestionObservation is what the backend knows about one answer.
type QuestionObservation struct {
	State  string // running, answered or failed
	Answer string
	Detail string
	Usage  *workflow.Usage
}

// scheduleQuestions answers open questions on every revision that could still
// have a supervisor to ask, including completed runs kept for inspection:
// being able to ask about finished work is the point.
func (e *Engine) scheduleQuestions(ctx context.Context, run workflow.Run, rev workflow.Revision) {
	for _, q := range rev.Questions {
		if q.Finished() {
			continue
		}
		id := q.ID
		e.launch(ctx, run.ID+"/"+rev.ID+"/question/"+id, func() error { return e.reconcileQuestion(ctx, run.ID, rev.ID, id) })
	}
}

func (e *Engine) reconcileQuestion(ctx context.Context, runID, revision, id string) error {
	run, err := e.Store.Get(ctx, runID)
	if err != nil {
		return err
	}
	rev := run.Revision(revision)
	if rev == nil {
		return nil
	}
	q := rev.Question(id)
	if q == nil || q.Finished() {
		return nil
	}
	fail := func(detail string) error {
		return e.finishQuestion(ctx, runID, revision, id, QuestionOutcome{State: workflow.QuestionFailed, Detail: detail})
	}

	backend, ok := e.Backend.(QuestionBackend)
	if !ok {
		return fail("this coordinator cannot answer questions")
	}
	attempt := rev.Attempt(q.Attempt)
	if attempt == nil {
		return fail("the attempt this question was about no longer exists")
	}
	a := assign(run, rev, attempt)
	// The answer reads the stage's worktree inside its VM. Once a run is
	// closed, cancelled or superseded that VM is gone, and a rewind is the
	// way back to a supervisor that can look.
	if !a.Revision.Runtime.Ready {
		return fail("the stage's VM is no longer running, so its supervisor cannot look at the work; rewind to reopen it")
	}
	// A question that has not started yet is checked against the ceiling
	// again: other work may have used the budget since it was asked.
	if q.State == workflow.QuestionPending {
		if exceeded, reason := run.Budget(); exceeded {
			return fail("the run reached its ceiling (" + reason + ") before this could be answered")
		}
	}
	obs, err := backend.Answer(ctx, a, *q)
	if err != nil {
		return err
	}
	switch obs.State {
	case workflow.QuestionAnswered:
		return e.finishQuestion(ctx, runID, revision, id, QuestionOutcome{State: workflow.QuestionAnswered, Answer: obs.Answer, Usage: obs.Usage})
	case workflow.QuestionFailed:
		return e.finishQuestion(ctx, runID, revision, id, QuestionOutcome{State: workflow.QuestionFailed, Detail: obs.Detail, Usage: obs.Usage})
	}
	if q.State == workflow.QuestionAnswering {
		return nil
	}
	_, err = e.update(ctx, runID, revision, "question.answering", func(_ *workflow.Run, v *workflow.Revision) error {
		current := v.Question(id)
		if current == nil || current.State != workflow.QuestionPending {
			return workflow.ErrConflict
		}
		current.State = workflow.QuestionAnswering
		return nil
	})
	return ignoreConflict(err)
}

// QuestionOutcome is a question's final state.
type QuestionOutcome struct {
	State  string
	Answer string
	Detail string
	Usage  *workflow.Usage
}

// finishQuestion records an answer exactly once. A question already finished
// is left alone, so a reconcile racing a restart cannot record it twice.
func (e *Engine) finishQuestion(ctx context.Context, runID, revision, id string, out QuestionOutcome) error {
	_, err := e.update(ctx, runID, revision, "question."+out.State, func(_ *workflow.Run, v *workflow.Revision) error {
		current := v.Question(id)
		if current == nil || current.Finished() {
			return workflow.ErrConflict
		}
		current.State, current.Answer, current.Detail, current.Usage = out.State, out.Answer, out.Detail, out.Usage
		current.AnsweredAt = e.now()
		return nil
	})
	return ignoreConflict(err)
}

// ignoreConflict treats a lost race as success. A question's transitions are
// guarded by its current state, so a conflict means another reconcile has
// already made the change.
func ignoreConflict(err error) error {
	if errors.Is(err, workflow.ErrConflict) {
		return nil
	}
	return err
}
