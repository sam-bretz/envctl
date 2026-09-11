// Package engine reconciles persisted workflow intent with durable guest jobs.
// A backend can report evidence; only the engine may accept a checkpoint.
package engine

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type Assignment struct {
	Run      workflow.Run
	Revision workflow.Revision
	Attempt  workflow.Attempt
	Child    string // node owning this child runtime; empty for the revision runtime
}
type Prepared struct {
	Runtime    workflow.RuntimeState
	SourcePins map[string]string
}
type Observation struct {
	State   string // missing, running, completed, failed, interrupted
	Result  *workflow.Result
	Detail  string // already redacted by the backend
	Session string // explicit harness session identity for durable continuation
	// Progress and Delivered are display state for running attempts. Delivered
	// lists user messages already submitted to an agent invocation.
	Progress  *workflow.Progress
	Delivered []workflow.Delivery
}

// All backend effects must be idempotent by revision/attempt ID. Poll must
// reconcile guest receipts, never infer success from a lost SSH connection.
// Publication is additionally serialized with user mutations by the store.
type Backend interface {
	Prepare(context.Context, Assignment) (Prepared, error)
	Readiness(context.Context, Assignment) ([]workflow.Probe, error)
	Start(context.Context, Assignment) error
	Poll(context.Context, Assignment) (Observation, error)
	Cancel(context.Context, Assignment) error
	Release(context.Context, Assignment) error
	Publish(context.Context, Assignment, workflow.Result) (map[string]string, error)
}

// RuntimeReporter is optional for backends with live service observations.
// Runtime identity cannot change as a side effect of refreshing UI metadata.
type RuntimeReporter interface {
	RuntimeStatus(context.Context, Assignment) (workflow.RuntimeState, error)
}

// BranchRuntimeBackend supports a distinct VM/stack/data namespace per branch.
// Existing fixture/serial backends retain their original execution contract.
type BranchRuntimeBackend interface{ IsolateBranches() bool }

func (e *Engine) childRequired(v *workflow.Revision, node string) bool {
	b, ok := e.Backend.(BranchRuntimeBackend)
	kind := v.Config.Workflow.Nodes[node].Kind
	return ok && b.IsolateBranches() && v.Config.Limits.Parallel > 1 && kind != "task" && kind != "plan"
}

type Engine struct {
	Store    *runstore.Store
	Backend  Backend
	Interval time.Duration
	Now      func() time.Time
	OnError  func(error)
	// ProgressInterval bounds how often activity-only progress is persisted
	// per attempt. Phase changes and message deliveries are written at once.
	ProgressInterval time.Duration
	mu               sync.Mutex
	busy             map[string]bool
	wg               sync.WaitGroup
}

func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// Run belongs to the daemon's single-owner lifetime. Shutdown cancels host
// transport only; guest jobs survive and are reconciled by the next daemon.
func (e *Engine) Run(ctx context.Context) error {
	if e.Store == nil || e.Backend == nil {
		return errors.New("engine requires a store and execution backend")
	}
	interval := e.Interval
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer e.wg.Wait()
	for {
		if err := e.Tick(ctx); err != nil && !errors.Is(err, context.Canceled) {
			e.report(err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
func (e *Engine) report(err error) {
	if e.OnError != nil {
		e.OnError(err)
	}
}

// Tick only schedules bounded reconciliation calls. Slow VM provisioning for
// one run cannot stop another run's jobs, reviews, or readiness work.
func (e *Engine) Tick(ctx context.Context) error {
	runs, err := e.Store.List(ctx)
	if err != nil {
		return err
	}
	for _, run := range runs {
		occupied := run.VMCount()
		for _, rev := range run.Revisions {
			for node, child := range rev.ChildRuntimes {
				if child == nil || !child.Runtime.OccupiesVM() {
					continue
				}
				node := node
				e.launch(ctx, run.ID+"/"+rev.ID+"/child/"+node, func() error { return e.ReconcileChild(ctx, run.ID, rev.ID, node) })
			}
			if rev.State == "completed" || rev.State == "needs-attention" {
				continue
			}
			if rev.State == "queued" {
				if rev.ID != run.CurrentRevision || occupied >= rev.Config.Limits.VMs {
					continue
				}
				_, err = e.update(ctx, run.ID, rev.ID, "runtime.reserved", func(r *workflow.Run, v *workflow.Revision) error {
					if v.State != "queued" || r.CurrentRevision != v.ID || r.VMCount() >= v.Config.Limits.VMs {
						return workflow.ErrConflict
					}
					v.State = "preparing"
					v.Runtime = workflow.RuntimeState{ID: "envctl-" + strings.ReplaceAll(v.ID, "_", "-"), Provider: v.Config.Runtime.Provider, Location: "local", State: "preparing"}
					return nil
				})
				if err != nil {
					if !errors.Is(err, workflow.ErrConflict) {
						e.report(err)
					}
					continue
				}
				occupied++
			}
			if slices.Contains([]string{"completed", "superseded", "cancelled"}, rev.State) && (!rev.Runtime.Ready && rev.Runtime.State != "preparing") {
				continue
			}
			if rev.Recovery != nil && rev.Recovery.RetryAt.After(e.now()) {
				continue
			}
			e.launch(ctx, run.ID+"/"+rev.ID, func() error { return e.Reconcile(ctx, run.ID, rev.ID) })
		}
	}
	return nil
}

func (e *Engine) update(ctx context.Context, runID, revision, kind string, fn func(*workflow.Run, *workflow.Revision) error) (*workflow.Run, error) {
	for i := 0; i < 8; i++ {
		run, err := e.Store.Get(ctx, runID)
		if err != nil {
			return nil, err
		}
		updated, err := e.Store.MutateRevision(ctx, runID, revision, run.Version, workflow.ID("engine"), kind, revision, func(r *workflow.Run) error {
			v := r.Revision(revision)
			if v == nil {
				return errors.New("revision disappeared")
			}
			return fn(r, v)
		})
		if !errors.Is(err, workflow.ErrConflict) {
			return updated, err
		}
	}
	return nil, workflow.ErrConflict
}
func assign(run *workflow.Run, rev *workflow.Revision, attempt *workflow.Attempt) Assignment {
	a := Assignment{Run: *workflow.Clone(run), Revision: workflow.Clone(*rev)}
	if attempt != nil {
		a.Attempt = workflow.Clone(*attempt)
		if rev.ChildRuntimes[attempt.Node] != nil {
			a = childAssignment(a, attempt.Node)
		}
	}
	return a
}

func childAssignment(a Assignment, node string) Assignment {
	child := a.Revision.ChildRuntimes[node]
	if child == nil {
		return a
	}
	a.Child = node
	a.Revision.Runtime = workflow.Clone(child.Runtime)
	a.Revision.Readiness = workflow.Clone(child.Readiness)
	a.Revision.ReadinessCheckedAt = child.ReadinessCheckedAt
	a.Revision.Recovery = workflow.Clone(child.Recovery)
	return a
}

// Reconcile is independently testable against durable backends and a real
// store. It performs at most one slow lifecycle operation per revision pass.
func (e *Engine) Reconcile(ctx context.Context, id, revision string) error {
	run, err := e.Store.Get(ctx, id)
	if err != nil {
		return err
	}
	rev := run.Revision(revision)
	if rev == nil {
		return errors.New("unknown revision")
	}
	if rev.Recovery != nil && rev.Recovery.RetryAt.After(e.now()) {
		return nil
	}
	if rev.State == "preparing" {
		prepared, err := e.Backend.Prepare(ctx, assign(run, rev, nil))
		if err != nil {
			return e.recover(ctx, id, revision, "preparation", err)
		}
		if !prepared.Runtime.Ready || prepared.Runtime.ID != rev.Runtime.ID || prepared.Runtime.DaemonID == "" {
			return e.recover(ctx, id, revision, "preparation", errors.New("runtime preparation lacks a matching ready VM and Docker identity"))
		}
		for _, repo := range rev.Config.Repositories {
			pin := prepared.SourcePins[repo.ID]
			_, parseErr := hex.DecodeString(pin)
			if parseErr != nil || strings.ToLower(pin) != pin || (len(pin) != 40 && len(pin) != 64) || (repo.BaseSHA != "" && repo.BaseSHA != pin) || (rev.SourcePins[repo.ID] != "" && rev.SourcePins[repo.ID] != pin) {
				return e.recover(ctx, id, revision, "preparation", fmt.Errorf("repository %s is missing its required immutable pin", repo.ID))
			}
		}
		_, err = e.update(ctx, id, revision, "runtime.ready", func(r *workflow.Run, v *workflow.Revision) error {
			// A rewind may arrive during provisioning. Retain ownership so the
			// next reconciliation releases the superseded guest; never activate it.
			v.Runtime = prepared.Runtime
			v.SourcePins = workflow.Clone(prepared.SourcePins)
			v.Recovery = nil
			if v.State == "preparing" && r.CurrentRevision == v.ID {
				v.State = "active"
			}
			return nil
		})
		return err
	}
	if slices.Contains([]string{"cancelled", "superseded"}, rev.State) {
		if rev.HasChildVMs() {
			return nil
		}
		for _, attempt := range rev.Attempts {
			if attempt.State == "cancelled" && rev.ChildRuntimes[attempt.Node] == nil {
				if err = e.Backend.Cancel(ctx, assign(run, rev, &attempt)); err != nil {
					return e.recover(ctx, id, revision, "cancellation", err)
				}
			}
		}
		if err = e.Backend.Release(ctx, assign(run, rev, nil)); err != nil {
			return e.recover(ctx, id, revision, "release", err)
		}
		_, err = e.update(ctx, id, revision, "runtime.stopped", func(_ *workflow.Run, v *workflow.Revision) error {
			v.Runtime.Ready = false
			v.Runtime.State = "stopped"
			v.Recovery = nil
			return nil
		})
		return err
	}
	if rev.State != "active" && rev.State != "draining" {
		return nil
	}
	if rev.ReadinessCheckedAt.IsZero() || e.now().Sub(rev.ReadinessCheckedAt) >= 30*time.Second {
		probes, err := e.Backend.Readiness(ctx, assign(run, rev, nil))
		if err != nil {
			return e.recover(ctx, id, revision, "readiness", err)
		}
		// Evidence must exist in coordinator-owned storage before a probe can
		// satisfy Plan. The backend cannot replace executable proof with prose.
		for _, probe := range probes {
			if _, err = e.Store.Artifact(probe.EvidenceDigest); err != nil {
				return e.recover(ctx, id, revision, "readiness", errors.New("readiness evidence is missing or corrupt"))
			}
		}
		var runtime *workflow.RuntimeState
		if reporter, ok := e.Backend.(RuntimeReporter); ok {
			state, inspectErr := reporter.RuntimeStatus(ctx, assign(run, rev, nil))
			if inspectErr != nil {
				return e.recover(ctx, id, revision, "runtime-status", inspectErr)
			}
			if state.ID != rev.Runtime.ID || state.DaemonID != rev.Runtime.DaemonID {
				return e.recover(ctx, id, revision, "runtime-status", errors.New("runtime identity changed during service inspection"))
			}
			runtime = &state
		}
		_, err = e.update(ctx, id, revision, "readiness.refreshed", func(_ *workflow.Run, v *workflow.Revision) error {
			if runtime != nil {
				v.Runtime = *runtime
			}
			for _, p := range probes {
				v.SetProbe(p)
			}
			v.ReadinessCheckedAt = e.now()
			v.Recovery = nil
			return nil
		})
		return err
	}
	for i := range rev.Attempts {
		a := &rev.Attempts[i]
		if rev.ChildRuntimes[a.Node] != nil {
			continue
		}
		acted, err := e.reconcileAttempt(ctx, run, rev, a)
		if acted || err != nil {
			return err
		}
	}
	if rev.State == "active" || rev.State == "draining" {
		ready := rev.ReadyNodes(e.now())
		if len(ready) == 0 && !rev.HasActive() && len(rev.ReadinessProblems(e.now(), rev.Requirements())) == 0 {
			// Budgets are per node. An exhausted node needs attention only once no
			// sibling with remaining budget is still waiting out its retry backoff.
			exhausted, count, retrying := "", 0, false
			for node := range rev.Config.Workflow.Nodes {
				if rev.State == "draining" && !slices.Contains(rev.DrainNodes, node) {
					continue
				}
				if _, done := rev.Checkpoints[node]; done {
					continue
				}
				attempts, backoff := 0, false
				for _, attempt := range rev.Attempts {
					if attempt.Node == node {
						attempts++
						backoff = backoff || (attempt.State == "failed" && attempt.RetryAt.After(e.now()))
					}
				}
				if attempts >= rev.Config.NodeLimits(node).MaxAttempts {
					if exhausted == "" || node < exhausted {
						exhausted, count = node, attempts
					}
				} else if backoff {
					retrying = true
				}
			}
			if exhausted != "" && !retrying {
				if err := e.recover(ctx, id, revision, "attempt-budget", fmt.Errorf("stage %s exhausted its configured %d attempts; revise the plan or budget to continue", exhausted, count)); err != nil {
					return err
				}
				_, err = e.update(ctx, id, revision, "run.needs-attention", func(_ *workflow.Run, v *workflow.Revision) error {
					if v.State != "active" && v.State != "draining" {
						return workflow.ErrConflict
					}
					v.State = "needs-attention"
					return nil
				})
				return err
			}
		}
		for _, node := range ready {
			_, err = e.update(ctx, id, revision, "attempt.started", func(r *workflow.Run, v *workflow.Revision) error {
				if r.CurrentRevision != revision && v.State != "draining" {
					return workflow.ErrConflict
				}
				if e.childRequired(v, node) && v.ChildRuntimes[node] == nil && r.VMCount() >= v.Config.Limits.VMs {
					return errCapacity
				}
				attempt, err := v.Begin(node, e.now())
				if err != nil {
					return err
				}
				if e.childRequired(v, node) {
					attempt.State = "preparing"
				}
				if e.childRequired(v, node) && v.ChildRuntimes[node] == nil {
					if v.ChildRuntimes == nil {
						v.ChildRuntimes = map[string]*workflow.ChildRuntime{}
					}
					v.ChildRuntimes[node] = &workflow.ChildRuntime{Runtime: workflow.RuntimeState{
						ID:       "envctl-" + strings.ReplaceAll(v.ID, "_", "-") + "-" + workflow.Digest(node)[:12],
						Provider: v.Config.Runtime.Provider, Location: "local", State: "preparing",
					}}
				}
				return nil
			})
			// Capacity exhaustion is normal; Begin enforces it atomically.
			if errors.Is(err, errCapacity) {
				continue // An existing branch runtime can still admit its retry.
			}
			if err != nil {
				break
			}
		}
	}
	return nil
}

func (e *Engine) verify(ctx context.Context, result workflow.Result) error {
	artifacts := slices.Clone(result.Artifacts)
	for _, a := range result.Sources {
		artifacts = append(artifacts, a)
	}
	for _, a := range result.SourceObjects {
		artifacts = append(artifacts, a)
	}
	for _, d := range result.Datasets {
		artifacts = append(artifacts, d.Source, d.Evidence)
	}
	for _, a := range artifacts {
		b, err := e.Store.Artifact(a.Digest)
		if err != nil || int64(len(b)) != a.Size {
			return fmt.Errorf("artifact %s is missing, corrupt, or has an incorrect size", a.Name)
		}
	}
	for repo, inputs := range result.MergeParents {
		bundle, err := e.Store.Artifact(result.Sources[repo].Digest)
		if err != nil {
			return fmt.Errorf("merge source %s is missing", repo)
		}
		parents := make([]string, 0, len(inputs))
		for _, pin := range inputs {
			parents = append(parents, pin)
		}
		slices.Sort(parents)
		if err := repository.VerifyMerge(ctx, bundle, result.Commits[repo], parents); err != nil {
			return fmt.Errorf("merge %s: %w", repo, err)
		}
	}
	if _, err := e.Store.Artifact(result.Review.EvidenceDigest); err != nil {
		return errors.New("supervisor evidence is missing or corrupt")
	}
	for _, c := range result.Checks {
		if _, err := e.Store.Artifact(c.EvidenceDigest); err != nil {
			return fmt.Errorf("check %s evidence is missing or corrupt", c.Name)
		}
	}
	return nil
}

func (e *Engine) propose(ctx context.Context, id, revision, attempt string, result workflow.Result) error {
	if err := e.verify(ctx, result); err != nil {
		return e.fail(ctx, id, revision, attempt, err.Error())
	}
	_, err := e.update(ctx, id, revision, "attempt.result", func(_ *workflow.Run, v *workflow.Revision) error {
		a := v.Attempt(attempt)
		if a == nil || a.State != "running" || (v.State != "active" && v.State != "draining") {
			return workflow.ErrConflict
		}
		before := workflow.Digest(v.DiscoveredRequirements)
		if err := v.RecordPlanResult(a.Node, result, e.now()); err != nil {
			return err
		}
		if before != workflow.Digest(v.DiscoveredRequirements) {
			v.ReadinessCheckedAt = time.Time{}
		}
		if v.State != "draining" && v.Config.Workflow.Nodes[a.Node].Kind == "plan" && len(v.ReadinessProblems(e.now(), v.Requirements())) > 0 {
			// Keep the work product while executable readiness catches up. It
			// is not a checkpoint and cannot dispatch downstream work.
			if a.Result != nil {
				return workflow.ErrConflict
			}
			a.Result = &result
			return nil
		}
		return v.Propose(attempt, result, e.now())
	})
	if err != nil && !errors.Is(err, workflow.ErrConflict) {
		return e.fail(ctx, id, revision, attempt, err.Error())
	}
	return err
}
func (e *Engine) fail(ctx context.Context, id, revision, attempt, detail string) error {
	if strings.TrimSpace(detail) == "" {
		detail = "guest attempt ended without completion evidence"
	}
	_, err := e.update(ctx, id, revision, "attempt.failed", func(_ *workflow.Run, v *workflow.Revision) error {
		if err := v.Fail(attempt, detail, e.now()); err != nil {
			return err
		}
		a := v.Attempt(attempt)
		a.RetryAt = e.now().Add(backoff(a.Number))
		return nil
	})
	return err
}
func backoff(failures int) time.Duration {
	return time.Second * time.Duration(1<<min(max(failures, 1), 8))
}
func (e *Engine) recover(ctx context.Context, id, revision, phase string, cause error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	// Backend errors are required to be redacted before crossing this boundary.
	artifact, err := e.Store.PutArtifact("recovery", "text/plain", []byte(cause.Error()))
	if err != nil {
		return err
	}
	_, err = e.update(ctx, id, revision, "recovery.required", func(_ *workflow.Run, v *workflow.Revision) error {
		failures := 1
		if v.Recovery != nil && v.Recovery.Phase == phase {
			failures = v.Recovery.Failures + 1
		}
		v.Recovery = &workflow.Recovery{Phase: phase, Detail: cause.Error(), EvidenceDigest: artifact.Digest, Failures: failures, RetryAt: e.now().Add(backoff(failures))}
		return nil
	})
	return err
}
