package engine

import (
	"context"
	"errors"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// reconcileAttempt shares checkpoint semantics between serial and child workers.
func (e *Engine) reconcileAttempt(ctx context.Context, run *workflow.Run, rev *workflow.Revision, a *workflow.Attempt) (bool, error) {
	id, revision := run.ID, rev.ID
	assignment := assign(run, rev, a)
	// Child readiness is independent of the parent's preparation stack.
	effective := assignment.Revision
	rev = &effective
	var err error
	if a.State == "running" {
		if a.Result != nil {
			if rev.State != "draining" && rev.Config.Workflow.Nodes[a.Node].Kind == "plan" && len(rev.ReadinessProblems(e.now(), rev.Requirements())) > 0 {
				return false, nil
			}
			return true, e.propose(ctx, id, revision, a.ID, *a.Result)
		}
		observation, err := e.Backend.Poll(ctx, assignment)
		if err != nil {
			return true, e.recoverAssignment(ctx, assignment, "reconnect", err)
		}
		if assignment.Child != "" && rev.Recovery != nil && (rev.Recovery.Phase == "reconnect" || rev.Recovery.Phase == "dispatch") {
			if _, err := e.update(ctx, id, revision, "child.reconnected", func(_ *workflow.Run, v *workflow.Revision) error {
				child := v.ChildRuntimes[assignment.Child]
				if child == nil || child.Runtime.ID != assignment.Revision.Runtime.ID {
					return workflow.ErrConflict
				}
				child.Recovery = nil
				return nil
			}); err != nil {
				return true, err
			}
		}
		if observation.Session != "" && a.Session != observation.Session {
			_, err = e.update(ctx, id, revision, "attempt.session", func(_ *workflow.Run, v *workflow.Revision) error {
				current := v.Attempt(a.ID)
				if current == nil {
					return workflow.ErrConflict
				}
				current.Session = observation.Session
				return nil
			})
			return true, err
		}
		switch observation.State {
		case "missing":
			kind := rev.Config.Workflow.Nodes[a.Node].Kind
			if kind != "task" && kind != "plan" && len(rev.ReadinessProblems(e.now(), rev.Requirements())) > 0 {
				return false, nil
			}
			if err = e.Backend.Start(ctx, assignment); err != nil {
				return true, e.recoverAssignment(ctx, assignment, "dispatch", err)
			}
		case "failed", "interrupted":
			return true, e.fail(ctx, id, revision, a.ID, observation.Detail)
		case "completed":
			if observation.Result == nil {
				return true, e.fail(ctx, id, revision, a.ID, "guest completion has no verifiable result")
			}
			return true, e.propose(ctx, id, revision, a.ID, *observation.Result)
		case "running": // Continue polling other parallel assignments.
		default:
			return true, e.recoverAssignment(ctx, assignment, "reconnect", errors.New("guest reported unknown execution state"))
		}
	}
	if a.State == "verifying" || (a.State == "awaiting-approval" && rev.State == "draining") {
		if a.Result == nil {
			return true, e.fail(ctx, id, revision, a.ID, "verified attempt has no result")
		}
		if err = e.verify(ctx, *a.Result); err != nil {
			return true, e.fail(ctx, id, revision, a.ID, err.Error())
		}
		if rev.State == "draining" {
			_, err = e.update(ctx, id, revision, "checkpoint.archived", func(_ *workflow.Run, v *workflow.Revision) error { return v.ArchiveDrained(a.ID, e.now()) })
			return true, err
		}
		if rev.Config.Workflow.Nodes[a.Node].Kind == "change" {
			_, err = e.Store.GuardedEffect(ctx, id, revision, workflow.ID("publication"), "checkpoint.published", func(r *workflow.Run) error {
				v := r.Current()
				current := v.Attempt(a.ID)
				if current == nil || current.State != "verifying" || current.Result == nil {
					return workflow.ErrConflict
				}
				// Require a matching approval before any external publication.
				if v.Config.Workflow.Nodes[current.Node].Gate == "human" && (current.Approval == nil || current.Approval.ResultDigest != current.Result.WorkDigest()) {
					return errors.New("publication has no matching human approval")
				}
				prs, publishErr := e.Backend.Publish(ctx, assign(r, v, current), *current.Result)
				if publishErr != nil {
					return publishErr
				}
				current.Result.PRs = prs
				_, acceptErr := v.Accept(current.ID, e.now())
				return acceptErr
			})
			if err != nil && !errors.Is(err, workflow.ErrConflict) {
				return true, e.recoverAssignment(ctx, assignment, "publication", err)
			}
			return true, err
		}
		_, err = e.update(ctx, id, revision, "checkpoint.accepted", func(_ *workflow.Run, v *workflow.Revision) error {
			if v.State == "draining" {
				return v.ArchiveDrained(a.ID, e.now())
			}
			if v.State != "active" {
				return workflow.ErrConflict
			}
			_, err := v.Accept(a.ID, e.now())
			return err
		})
		return true, err
	}
	return false, nil
}
