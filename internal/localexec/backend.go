// Package localexec connects durable workflow assignments to real VM services,
// repositories, and agent jobs. Coordinator-owned receipts describe progress;
// a lost transport is reconciled against the guest's job journal.
package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/publication"
	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Capabilities report operation evidence, never only credential presence.
// Publication brokers execute outside the worker's writable VM.
type Capabilities interface {
	Probe(context.Context, engine.Assignment, string) (bool, string, error)
	Publish(context.Context, engine.Assignment, workflow.Result) (map[string]string, error)
}
type Backend struct {
	Store        *runstore.Store
	Provider     vm.Provider
	Capabilities Capabilities
	memo         activityMemo
	previews     previewState
}

var _ engine.Backend = (*Backend)(nil)
var idPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,100}$`)

func New(store *runstore.Store) *Backend {
	return &Backend{Store: store, Provider: vm.NewLima(store.Dir), Capabilities: publication.New(store)}
}

func (b *Backend) IsolateBranches() bool { return true }

func assignmentRuntime(a engine.Assignment) error {
	kind := a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Kind
	if a.Revision.Config.Limits.Parallel > 1 && kind != "task" && kind != "plan" {
		child := a.Revision.ChildRuntimes[a.Attempt.Node]
		if a.Child != a.Attempt.Node || child == nil || !child.Runtime.Ready || !a.Revision.Runtime.Ready || child.Runtime.ID != a.Revision.Runtime.ID {
			return errors.New("parallel worker requires its own prepared child runtime")
		}
	}
	return nil
}
func (b *Backend) guest(a engine.Assignment) guestjob.Client {
	return guestjob.Client{Provider: b.Provider, Runtime: a.Revision.Runtime.ID}
}
func (b *Backend) repos(a engine.Assignment) repository.Guest {
	return repository.Guest{Executor: b.Provider, Runtime: a.Revision.Runtime.ID, Revision: a.Revision.ID}
}
func (b *Backend) stack(a engine.Assignment) gueststack.Client {
	return gueststack.Client{Executor: b.Provider, Runtime: a.Revision.Runtime.ID}
}
func (b *Backend) dir(a engine.Assignment) (string, error) {
	if !idPattern.MatchString(a.Run.ID) || !idPattern.MatchString(a.Revision.ID) {
		return "", errors.New("invalid execution identity")
	}
	dir := filepath.Join(b.Store.Dir, "execution", a.Run.ID, a.Revision.ID)
	if a.Child != "" {
		child := a.Revision.ChildRuntimes[a.Child]
		if !idPattern.MatchString(a.Child) || child == nil || child.Runtime.ID != a.Revision.Runtime.ID {
			return "", errors.New("child runtime is not the reserved assignment owner")
		}
		dir = filepath.Join(dir, "children", a.Child)
	}
	return dir, nil
}
func (b *Backend) Prepare(ctx context.Context, a engine.Assignment) (engine.Prepared, error) {
	c := a.Revision.Config
	if _, err := invocationPlugins(a); err != nil {
		return engine.Prepared{}, err
	}
	if c.Runtime.Provider != "lima" {
		return engine.Prepared{}, errors.New("local execution requires the Lima provider")
	}
	for _, h := range []workflow.Harness{c.Agents.Worker, c.Agents.Supervisor} {
		if _, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a)); err != nil || len(h.Command) > 0 {
			return engine.Prepared{}, errors.New("local execution requires a supported pinned harness")
		}
	}
	dir, err := b.dir(a)
	if err != nil {
		return engine.Prepared{}, err
	}
	spec := vm.DefaultSpec(a.Revision.Runtime.ID)
	spec.CPUs = c.Runtime.CPUs
	spec.MemoryGiB = c.Runtime.MemoryGiB
	spec.DiskGiB = c.Runtime.DiskGiB
	if c.Runtime.Image != "" {
		spec.Image = c.Runtime.Image
		spec.ImageDigest = c.Runtime.ImageDigest
	}
	instance, err := b.Provider.Ensure(ctx, spec)
	if err != nil {
		return engine.Prepared{}, errors.New("local VM provisioning or bootstrap failed")
	}
	if err = b.guest(a).Install(ctx); err != nil {
		return engine.Prepared{}, errors.New("guest runner installation failed")
	}
	pins := map[string]string{}
	resolver := repository.Resolver{Dir: filepath.Join(dir, "sources")}
	cache := repository.Cache{Dir: filepath.Join(b.Store.Dir, "source-cache")}
	for _, repo := range c.Repositories {
		if pin := a.Revision.SourcePins[repo.ID]; pin != "" {
			repo.BaseSHA = pin
		}
		if !filepath.IsAbs(repo.URL) && len(repo.URL) > 0 && repo.URL[0] == '.' {
			repo.URL = filepath.Join(c.Dir, repo.URL)
		}
		source, archive, err := cache.Prepare(ctx, resolver, repo)
		if err != nil {
			return engine.Prepared{}, fmt.Errorf("repository %s could not be prepared at its required pin", repo.ID)
		}
		if err = b.repos(a).Import(ctx, source, archive); err != nil {
			return engine.Prepared{}, fmt.Errorf("guest import failed for repository %s", repo.ID)
		}
		pins[repo.ID] = source.Repository.BaseSHA
	}
	installed := map[string]bool{}
	for _, h := range []workflow.Harness{c.Agents.Worker, c.Agents.Supervisor} {
		if installed[h.Kind] {
			continue
		}
		harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a))
		if err != nil {
			return engine.Prepared{}, err
		}
		if err = harness.Install(ctx); err != nil {
			return engine.Prepared{}, errors.New("pinned guest harness installation failed")
		}
		installed[h.Kind] = true
	}
	return engine.Prepared{SourcePins: pins, Runtime: workflow.RuntimeState{ID: instance.ID, Provider: "lima", Location: "local", Ready: true, State: "running", DaemonID: instance.DaemonID, ImageDigest: spec.ImageDigest}}, nil
}

// Assignment inputs come from accepted direct predecessors. Unequal commits
// at a join require explicit merge work; map iteration must never pick a winner.
func inputCommits(a engine.Assignment) (map[string]string, error) {
	pins := workflow.Clone(a.Revision.SourcePins)
	if pins == nil {
		pins = map[string]string{}
	}
	seen := map[string]string{}
	n := a.Revision.Config.Workflow.Nodes[a.Attempt.Node]
	for node, checkpoint := range a.Attempt.Inputs {
		cp, ok := a.Revision.Checkpoints[node]
		if !ok || cp.ID != checkpoint || cp.HistoricalOnly {
			return nil, errors.New("assignment checkpoint lineage changed")
		}
		for repo, pin := range cp.Result.Commits {
			if n.Join != nil && n.Join.Repositories[repo] != "" {
				continue
			}
			if prior, ok := seen[repo]; ok && prior != pin {
				return nil, errors.New("join requires an explicit verified repository merge")
			}
			seen[repo] = pin
			pins[repo] = pin
		}
	}
	parents, err := a.Revision.MergeInputs(a.Attempt.Node)
	if err != nil {
		return nil, err
	}
	for repo, inputs := range parents {
		for parent := range inputs {
			if a.Attempt.Inputs[parent] != a.Revision.Checkpoints[parent].ID {
				return nil, errors.New("merge assignment is missing its exact input lineage")
			}
		}
		pins[repo] = inputs[n.Join.Repositories[repo]]
		if pins[repo] == "" {
			return nil, errors.New("merge has no declared starting input")
		}
	}
	return pins, nil
}

func (b *Backend) worktrees(ctx context.Context, a engine.Assignment, attempt string, pins map[string]string) (map[string]string, error) {
	dirs := map[string]string{}
	for _, repo := range a.Revision.Config.Repositories {
		// Restore retained bundles before assigning a checkpoint from another VM.
		for _, cp := range a.Revision.Checkpoints {
			if cp.Result.Commits[repo.ID] != pins[repo.ID] {
				continue
			}
			if artifact, ok := cp.Result.Sources[repo.ID]; ok {
				raw, err := b.Store.Artifact(artifact.Digest)
				if err != nil {
					return nil, err
				}
				if err = b.repos(a).RestoreBundle(ctx, repo.ID, pins[repo.ID], bytes.NewReader(raw)); err != nil {
					return nil, errors.New("checkpoint source restoration failed")
				}
			}
			if artifact, ok := cp.Result.SourceObjects[repo.ID]; ok {
				raw, err := b.Store.Artifact(artifact.Digest)
				if err != nil {
					return nil, err
				}
				if err = b.repos(a).RestoreObjects(ctx, repo.ID, pins[repo.ID], bytes.NewReader(raw)); err != nil {
					return nil, errors.New("checkpoint submodule or LFS restoration failed")
				}
			}
		}
		dir, err := b.repos(a).Assign(ctx, attempt, repo.ID, pins[repo.ID])
		if err != nil {
			return nil, errors.New("guest worktree assignment failed")
		}
		dirs[repo.ID] = dir
	}
	return dirs, nil
}
func stackSpec(a engine.Assignment, id string, dirs map[string]string) gueststack.Spec {
	c := a.Revision.Config
	return gueststack.Spec{ID: id, Project: a.Revision.ID, Root: dirs[c.Repositories[0].ID], Stack: c.Stack, Expose: c.Expose}
}

func (b *Backend) Readiness(ctx context.Context, a engine.Assignment) ([]workflow.Probe, error) {
	now := time.Now().UTC()
	pins, err := readinessCommits(a)
	if err != nil {
		return nil, err
	}
	var probes []workflow.Probe
	for _, capability := range a.Revision.Requirements() {
		passed := false
		detail := "capability has no prepared invocation binding"
		var err error
		switch capability {
		case "repositories.readwrite":
			_, err = b.worktrees(ctx, a, "readiness", pins)
			passed = err == nil
			if passed {
				detail = "all pinned repositories have writable guest assignments"
			} else {
				detail = "pinned guest repository preparation failed"
			}
		case "workflow.checks":
			passed, detail = pluginChecksReady(a)
		case "dataset.restore":
			passed, detail, err = b.datasetReadiness(ctx, a)
		case "runtime.compose":
			if b.dataBusy(a) {
				detail = "dataset operation owns writer quiescence"
				break
			}
			// Do not replace an active stage's stack with the base-source stack.
			var p gueststack.Prepared
			p, err = b.currentStack(a)
			if errors.Is(err, os.ErrNotExist) {
				var dirs map[string]string
				dirs, err = b.worktrees(ctx, a, "readiness", pins)
				if err == nil {
					p, err = b.stack(a).Prepare(ctx, stackSpec(a, a.Revision.ID+"_setup", dirs))
				}
				if err == nil {
					err = b.stack(a).Up(ctx, p, 60)
				}
				if err == nil {
					err = b.saveStack(a, p)
				}
			}
			if err == nil {
				entries, e := b.stack(a).Status(ctx, p)
				err = e
				passed = e == nil && gueststack.Healthy(p, entries)
				if !passed && e == nil {
					err = b.stack(a).Up(ctx, p, 60)
					if err == nil {
						entries, err = b.stack(a).Status(ctx, p)
						passed = err == nil && gueststack.Healthy(p, entries)
					}
				}
			}
			if passed {
				detail = "guest Compose services are running and their configured health checks pass"
			} else {
				detail = "guest Compose preparation or health checks need attention"
			}
		case "harness.worker", "harness.supervisor":
			role := "worker"
			if capability == "harness.supervisor" {
				role = "supervisor"
			}
			passed, detail, err = b.harnessProbe(ctx, a, role, now)
		default:
			if b.Capabilities != nil {
				passed, detail, err = b.Capabilities.Probe(ctx, a, capability)
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			passed = false
		} // failures become explicit planning items
		evidence, e := b.artifact(a, "readiness."+capability, "application/json", mustJSON(map[string]any{"capability": capability, "passed": passed, "detail": detail, "runtime": a.Revision.Runtime.ID, "checked_at": now}))
		if e != nil {
			return nil, e
		}
		probes = append(probes, workflow.Probe{Capability: capability, Binding: "local:" + capability, ConfigDigest: workflow.Digest(a.Revision.Config), RuntimeID: a.Revision.Runtime.ID, Passed: passed, Detail: detail, EvidenceDigest: evidence.Digest, CheckedAt: now, ExpiresAt: now.Add(2 * time.Minute)})
	}
	return b.pluginProbes(ctx, a, probes)
}

// SourcePins identify the immutable repository bases. A restored revision must
// prepare readiness from its selected checkpoint, which may contain a changed
// Compose definition or Dockerfile. Using the original base would probe a
// different application from the one the next worker is assigned.
func readinessCommits(a engine.Assignment) (map[string]string, error) {
	if a.Child != "" && a.Attempt.ID != "" {
		return inputCommits(a)
	}
	pins := workflow.Clone(a.Revision.SourcePins)
	if a.Revision.FromCheckpoint == "" {
		return pins, nil
	}
	for _, cp := range a.Revision.Checkpoints {
		if cp.ID != a.Revision.FromCheckpoint {
			continue
		}
		if cp.HistoricalOnly {
			return nil, errors.New("historical checkpoint cannot supply readiness inputs")
		}
		for _, repo := range a.Revision.Config.Repositories {
			if cp.Result.Commits[repo.ID] == "" {
				return nil, errors.New("selected checkpoint lacks repository inputs")
			}
		}
		return workflow.Clone(cp.Result.Commits), nil
	}
	return nil, errors.New("selected readiness checkpoint is unavailable")
}
func mustJSON(v any) []byte { raw, _ := json.Marshal(v); return raw }

func (b *Backend) harnessProbe(ctx context.Context, a engine.Assignment, role string, now time.Time) (bool, string, error) {
	h := a.Revision.Config.Agents.Worker
	if role == "supervisor" {
		h = a.Revision.Config.Agents.Supervisor
	}
	harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a))
	if err != nil {
		return false, "harness adapter is unavailable", err
	}
	id := fmt.Sprintf("probe_%s_%s_%d", a.Revision.ID, role, now.Unix()/300)
	status, err := b.guest(a).Poll(ctx, id, 0)
	if err != nil {
		return false, "harness probe transport needs recovery", err
	}
	if status.State == "missing" {
		credential, err := agent.ResolveCredential(a.Revision.Config.Dir, h.Kind, h.Credential)
		if err != nil {
			return false, "harness connection is unavailable", nil
		}
		dir, _ := b.repos(a).SourceDir(a.Revision.Config.Repositories[0].ID)
		i := agent.Invocation{ID: id, Role: role, Directory: dir, Prompt: "This is an envctl connection probe. Do not modify files or call tools. Return accepted=true, a short confirmation summary, and an empty correction.", Schema: agent.AssessmentSchema(), Model: h.Model, TimeoutSeconds: 120}
		_, err = harness.Start(ctx, i, credential)
		return false, "authenticating the harness with a real minimal invocation", err
	}
	if status.State != "completed" {
		if status.State == "pending" {
			_, err = b.guest(a).Reconcile(ctx, id)
			return false, "reconciling the original harness probe start intent", err
		}
		return false, "harness connection probe has not completed successfully", nil
	}
	raw, err := harness.Result(ctx, id)
	if err != nil {
		return false, "harness probe result is unavailable", err
	}
	result, err := agent.ParseAssessment(clean(a, raw))
	return err == nil && result.Accepted, "harness authenticated and returned its structured probe response", err
}

func (b *Backend) Release(ctx context.Context, a engine.Assignment) error {
	if _, err := b.dir(a); err != nil {
		return err
	}
	if err := b.cleanupPlugins(ctx, a); err != nil {
		return err
	}
	b.previewManager().Close(a.Revision.Runtime.ID)
	if err := b.Provider.Destroy(ctx, a.Revision.Runtime.ID); err != nil && !errors.Is(err, vm.ErrMissing) {
		return errors.New("owned local VM release failed")
	}
	return nil
}
func (b *Backend) Publish(ctx context.Context, a engine.Assignment, r workflow.Result) (map[string]string, error) {
	if b.Capabilities == nil {
		return nil, errors.New("PR publication binding is not prepared")
	}
	return b.Capabilities.Publish(ctx, a, r)
}
func (b *Backend) Cancel(ctx context.Context, a engine.Assignment) error {
	if _, err := b.dir(a); err != nil {
		return err
	}
	cancel := func(id string) error {
		status, err := b.guest(a).Poll(ctx, id, 0)
		if err != nil {
			return err
		}
		if status.State == "missing" || status.State == "completed" || status.State == "failed" || status.State == "cancelled" || status.State == "timed-out" {
			return nil
		}
		_, err = b.guest(a).Cancel(ctx, id)
		return err
	}
	ids := []string{a.Attempt.ID + "_worker", a.Attempt.ID + "_supervisor"}
	if r, err := b.load(a); err == nil {
		// Live steering resumes a role in later generations; stop all of them.
		for _, role := range []string{"worker", "supervisor"} {
			for _, id := range r.jobs(role) {
				if !slices.Contains(ids, id) {
					ids = append(ids, id)
				}
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, id := range ids {
		if err := cancel(id); err != nil {
			return err
		}
	}
	for i := range a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Checks {
		if err := cancel(fmt.Sprintf("%s_check_%d", a.Attempt.ID, i)); err != nil {
			return err
		}
	}
	return nil
}
func writable(node workflow.Node, repo string) bool {
	return slices.Contains(node.Writes, "*") || slices.Contains(node.Writes, repo)
}

func (b *Backend) RuntimeStatus(ctx context.Context, a engine.Assignment) (workflow.RuntimeState, error) {
	state := workflow.Clone(a.Revision.Runtime)
	p, err := b.currentStack(a)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	entries, err := b.stack(a).Status(ctx, p)
	if err != nil {
		return state, err
	}
	state.ComposeDigest = p.Digest
	state.Services = nil
	for _, entry := range entries {
		status := entry.State
		if entry.Health != "" {
			status += "/" + entry.Health
		}
		state.Services = append(state.Services, workflow.Service{Name: entry.Service, State: status})
	}
	state.Services = withServiceURLs(state.Services, entries)
	return state, nil
}
