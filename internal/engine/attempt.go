package engine

import (
	"context"
	"errors"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func (e *Engine) progressInterval() time.Duration {
	if e.ProgressInterval > 0 {
		return e.ProgressInterval
	}
	return 3 * time.Second
}

// observe records live display state before the observation's state is acted
// on. Deliveries are durable immediately so the UI can show that steering
// reached the agent; activity-only progress is throttled so the store is
// written with phases and deliveries, not with every harness event.
func (e *Engine) observe(ctx context.Context, runID, revision string, a *workflow.Attempt, o Observation) (bool, error) {
	same := func(x workflow.Delivery) func(workflow.Delivery) bool {
		return func(y workflow.Delivery) bool {
			return x.Message == y.Message && x.Role == y.Role && x.Generation == y.Generation
		}
	}
	var deliveries []workflow.Delivery
	for _, d := range o.Delivered {
		if !slices.ContainsFunc(a.Steering, same(d)) && !slices.ContainsFunc(deliveries, same(d)) {
			deliveries = append(deliveries, d)
		}
	}
	var progress *workflow.Progress
	if o.State == "running" && o.Progress != nil {
		p := *o.Progress
		p.Activity = workflow.BoundActivity(p.Activity)
		p.Usage = o.Usage
		if !workflow.SameProgress(a.Progress, &p) {
			activityOnly := a.Progress != nil && a.Progress.Phase == p.Phase && a.Progress.Detail == p.Detail && a.Progress.Generation == p.Generation
			if !activityOnly || e.now().Sub(a.Progress.UpdatedAt) >= e.progressInterval() {
				progress = &p
			}
		}
	}
	if len(deliveries) == 0 && progress == nil {
		return false, nil
	}
	// Progress lives outside the versioned run document: frequent activity
	// must not make clients' version-fenced commands stale.
	if progress != nil {
		progress.UpdatedAt = e.now()
		if err := e.Store.SetProgress(ctx, runID, a.ID, *progress); err != nil {
			return true, err
		}
	}
	if len(deliveries) == 0 {
		return true, nil
	}
	_, err := e.update(ctx, runID, revision, "attempt.steering", func(_ *workflow.Run, v *workflow.Revision) error {
		current := v.Attempt(a.ID)
		if current == nil || current.State != "running" {
			return workflow.ErrConflict
		}
		for _, d := range deliveries {
			if !slices.ContainsFunc(current.Steering, same(d)) {
				current.Steering = append(current.Steering, d)
			}
		}
		return nil
	})
	return true, err
}

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
		// Record final usage before acting on a stopped attempt, so the run's
		// total and its ceiling include every finished job.
		// A job stopped before reporting usage keeps its last live figure, so a
		// cancelled attempt cannot drop out of the run total.
		var final *workflow.Usage
		switch {
		case observation.Usage != nil:
			final = observation.Usage
		case a.Progress != nil:
			final = a.Progress.Usage
		}
		var usage workflow.Usage
		if final != nil {
			usage = *final
			usage.Estimated = false
		}
		if observation.State != "running" && final != nil && (a.Usage == nil || *a.Usage != usage) {
			_, err = e.update(ctx, id, revision, "attempt.usage", func(_ *workflow.Run, v *workflow.Revision) error {
				current := v.Attempt(a.ID)
				if current == nil || current.State != "running" {
					return workflow.ErrConflict
				}
				current.Usage = &usage
				return nil
			})
			return true, err
		}
		if fresh := newNotes(rev, observation.Notes); len(fresh) > 0 {
			_, err = e.update(ctx, id, revision, "revision.notes", func(_ *workflow.Run, v *workflow.Revision) error {
				added := false
				for _, n := range fresh {
					added = v.AddNote(n) || added
				}
				if !added {
					return workflow.ErrConflict
				}
				return nil
			})
			if err != nil && !errors.Is(err, workflow.ErrConflict) {
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
		if handled, err := e.observe(ctx, id, revision, a, observation); handled || err != nil {
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
				if v.Undecided() {
					return errors.New("a variation cannot publish before one is chosen")
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
				if _, err := v.Accept(current.ID, e.now()); err != nil {
					return err
				}
				r.AppendTrackerLog(v.Config.Tracker, workflow.TrackerKindStageCompleted, v.ID, current.Node, current.ID, "", e.now())
				// The pull request is the run's visible outcome, so it is
				// logged as its own entry rather than buried in the stage's.
				if len(prs) > 0 {
					r.AppendTrackerLog(v.Config.Tracker, workflow.TrackerKindPublished, v.ID, current.Node, current.ID, "", e.now())
					v.AddNote(workflow.Note{At: e.now(), Kind: workflow.NotePublished, Node: current.Node, Detail: prLinks(prs)})
				}
				return nil
			})
			if err != nil && !errors.Is(err, workflow.ErrConflict) {
				return true, e.recoverAssignment(ctx, assignment, "publication", err)
			}
			return true, err
		}
		_, err = e.update(ctx, id, revision, "checkpoint.accepted", func(run *workflow.Run, v *workflow.Revision) error {
			if v.State == "draining" {
				return v.ArchiveDrained(a.ID, e.now())
			}
			if v.State != "active" {
				return workflow.ErrConflict
			}
			cp, err := v.Accept(a.ID, e.now())
			if err != nil {
				return err
			}
			run.AppendTrackerLog(v.Config.Tracker, workflow.TrackerKindStageCompleted, v.ID, a.Node, a.ID, "", e.now())
			// Proposals become variations in the mutation that accepts the
			// stage, so they branch exactly once. Branch appends revisions, so
			// nothing may use v after it.
			if shouldBranch(run, v, a.Node, cp) {
				return run.Branch(v.ID, a.Node, cp.Result.Review.Variations, e.now())
			}
			return nil
		})
		return true, err
	}
	return false, nil
}

// shouldBranch reports whether an accepted stage's proposals should become
// variations: the stage opted in, the supervisor proposed enough of them, and
// this revision is not already a variation.
func shouldBranch(run *workflow.Run, v *workflow.Revision, node string, cp workflow.Checkpoint) bool {
	return v.Config.Workflow.Nodes[node].Variations >= workflow.MinVariations &&
		len(cp.Result.Review.Variations) >= workflow.MinVariations &&
		v.Variant == nil && run.CurrentRevision == v.ID
}

// prLinks lists pull requests in a stable order, one repository each.
func prLinks(prs map[string]string) string {
	keys := make([]string, 0, len(prs))
	for k := range prs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	links := make([]string, 0, len(keys))
	for _, k := range keys {
		links = append(links, k+": "+prs[k])
	}
	return strings.Join(links, ", ")
}

// newNotes filters reported notes down to those the revision has not kept,
// so a note reported on every poll causes one write.
func newNotes(rev *workflow.Revision, reported []workflow.Note) []workflow.Note {
	var fresh []workflow.Note
	for _, n := range reported {
		probe := workflow.Revision{Notes: rev.Notes}
		if probe.AddNote(n) {
			fresh = append(fresh, n)
		}
	}
	return fresh
}
