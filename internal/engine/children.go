package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

var errCapacity = errors.New("runtime capacity reached")

func (e *Engine) launch(ctx context.Context, key string, work func() error) {
	e.mu.Lock()
	if e.busy == nil {
		e.busy = map[string]bool{}
	}
	if e.busy[key] {
		e.mu.Unlock()
		return
	}
	e.busy[key] = true
	e.wg.Add(1)
	e.mu.Unlock()
	go func() {
		defer e.wg.Done()
		defer func() { e.mu.Lock(); delete(e.busy, key); e.mu.Unlock() }()
		if err := work(); err != nil && !errors.Is(err, workflow.ErrConflict) && ctx.Err() == nil {
			e.report(err)
		}
	}()
}

func (e *Engine) recoverAssignment(ctx context.Context, a Assignment, phase string, cause error) error {
	if a.Child == "" {
		return e.recover(ctx, a.Run.ID, a.Revision.ID, phase, cause)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	artifact, err := e.Store.PutArtifact("recovery", "text/plain", []byte(cause.Error()))
	if err != nil {
		return err
	}
	_, err = e.update(ctx, a.Run.ID, a.Revision.ID, "child.recovery", func(_ *workflow.Run, v *workflow.Revision) error {
		child := v.ChildRuntimes[a.Child]
		if child == nil || child.Runtime.ID != a.Revision.Runtime.ID {
			return workflow.ErrConflict
		}
		failures := 1
		if child.Recovery != nil && child.Recovery.Phase == phase {
			failures = child.Recovery.Failures + 1
		}
		child.Recovery = &workflow.Recovery{Phase: phase, Detail: cause.Error(), EvidenceDigest: artifact.Digest, Failures: failures, RetryAt: e.now().Add(backoff(failures))}
		return nil
	})
	return err
}

// ReconcileChild has a separate coordinator lock and recovery clock. Slow VM
// startup, plugin preparation or transport reconnection never locks siblings.
func (e *Engine) ReconcileChild(ctx context.Context, id, revision, node string) error {
	run, err := e.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	v := run.Revision(revision)
	if v == nil {
		return errors.New("child revision is missing")
	}
	child := v.ChildRuntimes[node]
	if child == nil || !child.Runtime.OccupiesVM() {
		return nil
	}
	var latest *workflow.Attempt
	for i := range v.Attempts {
		if v.Attempts[i].Node == node {
			latest = &v.Attempts[i]
		}
	}
	a := childAssignment(assign(run, v, nil), node)
	if latest != nil {
		a.Attempt = workflow.Clone(*latest)
	}
	_, checkpointed := v.Checkpoints[node]
	// Keep the last live application runtime(s) of a completed current run
	// available for inspection, matching the serial runtime lifetime. Rewind
	// or explicit cancellation releases them. Intermediate checkpoints release
	// capacity for downstream work after their evidence has been retained.
	release := (checkpointed && (v.State != "completed" || run.CurrentRevision != revision)) || slices.Contains([]string{"cancelled", "superseded"}, v.State)
	// Explicit cancellation/rewind cleanup bypasses a previous operation's
	// retry timer. Release failures retain their own backoff and reservation.
	if child.Recovery != nil && child.Recovery.RetryAt.After(e.now()) && (!release || child.Recovery.Phase == "release") {
		return nil
	}
	if release {
		if latest != nil && latest.State == "cancelled" && child.Runtime.Ready {
			if err := e.Backend.Cancel(ctx, a); err != nil {
				return e.recoverAssignment(ctx, a, "cancellation", err)
			}
		}
		if err := e.Backend.Release(ctx, a); err != nil {
			return e.recoverAssignment(ctx, a, "release", err)
		}
		_, err := e.update(ctx, id, revision, "child.released", func(_ *workflow.Run, v *workflow.Revision) error {
			c := v.ChildRuntimes[node]
			if c == nil || c.Runtime.ID != child.Runtime.ID {
				return workflow.ErrConflict
			}
			c.Runtime.Ready, c.Runtime.State, c.Recovery = false, "released", nil
			return nil
		})
		return err
	}
	if v.State != "active" && v.State != "draining" {
		return nil
	}
	if child.Runtime.State == "preparing" {
		prepared, err := e.Backend.Prepare(ctx, a)
		if err != nil {
			return e.recoverAssignment(ctx, a, "preparation", err)
		}
		if !prepared.Runtime.Ready || prepared.Runtime.ID != child.Runtime.ID || prepared.Runtime.DaemonID == "" || workflow.Digest(prepared.SourcePins) != workflow.Digest(v.SourcePins) || (v.Runtime.ImageDigest != "" && prepared.Runtime.ImageDigest != v.Runtime.ImageDigest) {
			return e.recoverAssignment(ctx, a, "preparation", errors.New("child runtime does not match its reserved VM, image or repository identities"))
		}
		_, err = e.update(ctx, id, revision, "child.ready", func(r *workflow.Run, v *workflow.Revision) error {
			// Independently executing branches must never report the same VM or
			// Docker daemon, including across an overlapping rewind revision.
			for _, rev := range r.Revisions {
				if rev.Runtime.ID == prepared.Runtime.ID || (rev.Runtime.DaemonID != "" && rev.Runtime.DaemonID == prepared.Runtime.DaemonID) {
					return errors.New("child runtime aliases a revision runtime")
				}
				for key, other := range rev.ChildRuntimes {
					if rev.ID == revision && key == node {
						continue
					}
					if other != nil && (other.Runtime.ID == prepared.Runtime.ID || (other.Runtime.DaemonID != "" && other.Runtime.DaemonID == prepared.Runtime.DaemonID)) {
						return errors.New("child runtime aliases another branch")
					}
				}
			}
			c := v.ChildRuntimes[node]
			if c == nil || c.Runtime.ID != child.Runtime.ID {
				return workflow.ErrConflict
			}
			c.Runtime, c.Recovery = prepared.Runtime, nil
			return nil
		})
		if err != nil && !errors.Is(err, workflow.ErrConflict) {
			return e.recoverAssignment(ctx, a, "preparation", err)
		}
		return err
	}
	if !child.Runtime.Ready {
		return nil
	}
	if child.ReadinessCheckedAt.IsZero() || e.now().Sub(child.ReadinessCheckedAt) >= 30*time.Second {
		probes, err := e.Backend.Readiness(ctx, a)
		if err != nil {
			return e.recoverAssignment(ctx, a, "readiness", err)
		}
		for _, probe := range probes {
			if _, err := e.Store.Artifact(probe.EvidenceDigest); err != nil {
				return e.recoverAssignment(ctx, a, "readiness", errors.New("child readiness evidence missing or corrupt"))
			}
		}
		state := child.Runtime
		if reporter, ok := e.Backend.(RuntimeReporter); ok {
			state, err = reporter.RuntimeStatus(ctx, a)
			if err != nil {
				return e.recoverAssignment(ctx, a, "runtime-status", err)
			}
			if state.ID != child.Runtime.ID || state.DaemonID != child.Runtime.DaemonID {
				return e.recoverAssignment(ctx, a, "runtime-status", errors.New("child identity changed during inspection"))
			}
		}
		_, err = e.update(ctx, id, revision, "child.readiness", func(_ *workflow.Run, v *workflow.Revision) error {
			c := v.ChildRuntimes[node]
			if c == nil || c.Runtime.ID != child.Runtime.ID {
				return workflow.ErrConflict
			}
			c.Runtime, c.Readiness, c.ReadinessCheckedAt, c.Recovery = state, workflow.Clone(probes), e.now(), nil
			return nil
		})
		return err
	}
	if latest == nil {
		return fmt.Errorf("child %s has no assignment", node)
	}
	if latest.State == "preparing" {
		if len(a.Revision.ReadinessProblems(e.now(), a.Revision.Requirements())) > 0 {
			return nil
		}
		_, err := e.update(ctx, id, revision, "child.admitted", func(r *workflow.Run, v *workflow.Revision) error {
			current := v.Attempt(latest.ID)
			if current == nil || current.State != "preparing" || (v.State != "active" && v.State != "draining") {
				return workflow.ErrConflict
			}
			bound := assign(r, v, current)
			if !bound.Revision.Runtime.Ready || len(bound.Revision.ReadinessProblems(e.now(), bound.Revision.Requirements())) > 0 {
				return workflow.ErrConflict
			}
			current.State, current.UpdatedAt = "running", e.now()
			return nil
		})
		return err
	}
	_, err = e.reconcileAttempt(ctx, run, v, latest)
	return err
}
