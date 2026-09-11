package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// This backend tests scheduling/transactions with actual stored evidence. Real
// process and VM acceptance lives in guestjob/runtime integration tests.
type fixtureBackend struct {
	store            *runstore.Store
	starts           map[string]int
	observations     map[string]Observation
	unready          bool
	lostAck          bool
	published        int
	released         int
	prepareHook      func()
	now              func() time.Time
	missing          map[string]bool
	planRequirements []workflow.Requirement
}

func (b *fixtureBackend) Prepare(_ context.Context, a Assignment) (Prepared, error) {
	if b.prepareHook != nil {
		b.prepareHook()
	}
	runtime := a.Revision.Runtime
	runtime.Ready, runtime.State, runtime.DaemonID = true, "running", "daemon-"+runtime.ID
	return Prepared{Runtime: runtime, SourcePins: map[string]string{"app": strings.Repeat("a", 40)}}, nil
}
func (b *fixtureBackend) Readiness(_ context.Context, a Assignment) ([]workflow.Probe, error) {
	artifact, err := b.store.PutArtifact("probe", "text/plain", []byte("executed readiness fixture"))
	if err != nil {
		return nil, err
	}
	var probes []workflow.Probe
	for _, capability := range a.Revision.Requirements() {
		probes = append(probes, workflow.Probe{Capability: capability, Binding: "fixture@1", ConfigDigest: workflow.Digest(a.Revision.Config), RuntimeID: a.Revision.Runtime.ID, Passed: !b.unready && !b.missing[capability], Detail: "fixture", EvidenceDigest: artifact.Digest, CheckedAt: b.now(), ExpiresAt: b.now().Add(time.Hour)})
	}
	return probes, nil
}
func (b *fixtureBackend) Start(_ context.Context, a Assignment) error {
	b.starts[a.Attempt.ID]++
	if _, exists := b.observations[a.Attempt.ID]; !exists {
		b.observations[a.Attempt.ID] = Observation{State: "completed", Result: b.result(a)}
	}
	if b.lostAck {
		b.lostAck = false
		return errors.New("transport disconnected after guest accepted job")
	}
	return nil
}
func (b *fixtureBackend) Poll(_ context.Context, a Assignment) (Observation, error) {
	if o, ok := b.observations[a.Attempt.ID]; ok {
		return o, nil
	}
	return Observation{State: "missing"}, nil
}
func (b *fixtureBackend) Cancel(_ context.Context, a Assignment) error {
	b.observations[a.Attempt.ID] = Observation{State: "failed", Detail: "cancelled"}
	return nil
}
func (b *fixtureBackend) Release(context.Context, Assignment) error { b.released++; return nil }
func (b *fixtureBackend) Publish(_ context.Context, _ Assignment, result workflow.Result) (map[string]string, error) {
	b.published++
	return map[string]string{"app": "https://example.test/pr/1"}, nil
}
func (b *fixtureBackend) result(a Assignment) *workflow.Result {
	r := &workflow.Result{Summary: "Fixture result", Commits: map[string]string{"app": strings.Repeat("b", 40)}}
	if a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Kind == "plan" {
		r.Requirements = workflow.Clone(b.planRequirements)
	}
	for _, output := range a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Outputs {
		artifact, _ := b.store.PutArtifact(output, "text/plain", []byte("verified "+output))
		r.Artifacts = append(r.Artifacts, artifact)
	}
	evidence, _ := b.store.PutArtifact("evidence", "text/plain", []byte("executed test and review fixture"))
	if a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Kind == "qa" {
		r.Checks = []workflow.CheckResult{{Name: "unit", Passed: true, EvidenceDigest: evidence.Digest, CommitsDigest: workflow.Digest(r.Commits)}}
	}
	r.Review = workflow.Review{Accepted: true, Summary: "Fixture reviewed", EvidenceDigest: evidence.Digest, ResultDigest: r.WorkDigest()}
	return r
}

type harness struct {
	t       *testing.T
	engine  *Engine
	backend *fixtureBackend
	id, rev string
	now     time.Time
}

func setup(t *testing.T) *harness {
	t.Helper()
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c, err := workflow.Parse([]byte("version: 2\nproject: test\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, err := workflow.NewRun("test", "exercise workflow", "developer", c, now)
	if err != nil {
		t.Fatal(err)
	}
	run.Current().State = "preparing"
	run.Current().Runtime = workflow.RuntimeState{ID: "envctl-test", Provider: "lima", Location: "local", State: "preparing"}
	if _, err = store.Create(context.Background(), "create", "fixture", run); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, id: run.ID, rev: run.CurrentRevision, now: now}
	b := &fixtureBackend{store: store, starts: map[string]int{}, observations: map[string]Observation{}, now: func() time.Time { return h.now }}
	h.backend = b
	h.engine = &Engine{Store: store, Backend: b, Now: b.now}
	return h
}
func (h *harness) step() {
	h.t.Helper()
	h.now = h.now.Add(time.Second)
	if err := h.engine.Reconcile(context.Background(), h.id, h.rev); err != nil && !errors.Is(err, workflow.ErrConflict) {
		h.t.Fatal(err)
	}
}
func (h *harness) run() *workflow.Run {
	h.t.Helper()
	run, err := h.engine.Store.Get(context.Background(), h.id)
	if err != nil {
		h.t.Fatal(err)
	}
	return run
}
func (h *harness) until(fn func(*workflow.Revision) bool) {
	h.t.Helper()
	for i := 0; i < 100; i++ {
		if fn(h.run().Revision(h.rev)) {
			return
		}
		h.step()
	}
	h.t.Fatalf("condition never reached: %+v", h.run().Revision(h.rev))
}
func (h *harness) mutate(fn func(*workflow.Run) error) {
	h.t.Helper()
	r := h.run()
	if _, err := h.engine.Store.Mutate(context.Background(), h.id, r.Version, workflow.ID("user"), "user.action", nil, fn); err != nil {
		h.t.Fatal(err)
	}
}

func TestCheckpointAdmissionAndPublication(t *testing.T) {
	h := setup(t)
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.State == "awaiting-approval" {
				return true
			}
		}
		return false
	})
	if h.backend.published != 0 {
		t.Fatal("published before approval")
	}
	r := h.run()
	v := r.Current()
	a := v.Attempts[len(v.Attempts)-1]
	h.mutate(func(r *workflow.Run) error { return r.Approve(h.rev, a.ID, "developer", a.Result.WorkDigest(), h.now) })
	h.until(func(v *workflow.Revision) bool { return v.State == "completed" })
	for i := 0; i < 3; i++ {
		h.step()
	}
	if h.backend.published != 1 || len(h.run().Current().Checkpoints) != 6 {
		t.Fatal("missing or duplicate publication/checkpoints")
	}
}

func TestPlanHoldsResultUntilExecutableReadiness(t *testing.T) {
	h := setup(t)
	h.backend.unready = true
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.Node == "plan" && a.Result != nil {
				return true
			}
		}
		return false
	})
	for i := 0; i < 5; i++ {
		h.step()
	}
	v := h.run().Current()
	if _, exists := v.Checkpoints["plan"]; exists {
		t.Fatal("Plan accepted failed probes")
	}
	for _, a := range v.Attempts {
		if a.Node != "task" && a.Node != "plan" {
			t.Fatal("downstream work escaped Plan")
		}
	}
	h.backend.unready = false
	h.now = h.now.Add(time.Minute)
	h.until(func(v *workflow.Revision) bool { _, ok := v.Checkpoints["plan"]; return ok })
}

func TestReconnectAfterLostAcknowledgementDoesNotRestartJob(t *testing.T) {
	h := setup(t)
	h.backend.lostAck = true
	h.until(func(v *workflow.Revision) bool { return v.Recovery != nil })
	old := h.engine
	// Recreate the coordinator while the backend keeps its durable job receipt.
	h.engine = &Engine{Store: old.Store, Backend: h.backend, Now: old.Now}
	h.until(func(v *workflow.Revision) bool { _, ok := v.Checkpoints["task"]; return ok })
	for _, count := range h.backend.starts {
		if count != 1 {
			t.Fatalf("job executed %d times", count)
		}
	}
}

func TestCorruptArtifactFailsAndRetries(t *testing.T) {
	h := setup(t)
	h.until(func(v *workflow.Revision) bool { return len(v.Attempts) > 0 })
	a := h.run().Current().Attempts[0]
	bad := h.backend.result(assign(h.run(), h.run().Current(), &a))
	bad.Artifacts[0].Digest = strings.Repeat("f", 64)
	bad.Review.ResultDigest = bad.WorkDigest()
	h.backend.observations[a.ID] = Observation{State: "completed", Result: bad}
	h.step()
	if h.run().Current().Attempts[0].State != "failed" {
		t.Fatal("corrupt artifact accepted")
	}
	h.until(func(v *workflow.Revision) bool { _, ok := v.Checkpoints["task"]; return ok })
	if h.run().Current().Attempts[1].Number != 2 {
		t.Fatal("retry lost lineage")
	}
}

func TestSourceCheckpointRequiresRetainedBundle(t *testing.T) {
	h := setup(t)
	h.until(func(v *workflow.Revision) bool { return len(v.Attempts) > 0 })
	a := h.run().Current().Attempts[0]
	result := h.backend.result(assign(h.run(), h.run().Current(), &a))
	result.Sources = map[string]workflow.Artifact{"app": {Name: "source", MediaType: "application/x-git-bundle", Digest: strings.Repeat("e", 64), Size: 20}}
	result.Review.ResultDigest = result.WorkDigest()
	h.backend.observations[a.ID] = Observation{State: "completed", Result: result}
	h.step()
	if h.run().Current().Attempts[0].State != "failed" {
		t.Fatal("missing source bundle accepted")
	}
}

func TestSourceObjectCompanionMustBeRetainedAndVerified(t *testing.T) {
	h := setup(t)
	proof, err := h.engine.Store.PutArtifact("review", "text/plain", []byte("review"))
	if err != nil {
		t.Fatal(err)
	}
	objects, err := h.engine.Store.PutArtifact("source-objects", "application/vnd.envctl.git-objects+tar", []byte("companion bytes"))
	if err != nil {
		t.Fatal(err)
	}
	r := workflow.Result{SourceObjects: map[string]workflow.Artifact{"app": objects}, Review: workflow.Review{EvidenceDigest: proof.Digest}}
	if err := h.engine.verify(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	objects.Size++
	r.SourceObjects["app"] = objects
	if err := h.engine.verify(context.Background(), r); err == nil {
		t.Fatal("incorrect companion size accepted")
	}
	objects.Size--
	objects.Digest = strings.Repeat("e", 64)
	r.SourceObjects["app"] = objects
	if err := h.engine.verify(context.Background(), r); err == nil {
		t.Fatal("missing companion accepted")
	}
}

func TestDatasetCheckpointRequiresRetainedDumpAndEvidence(t *testing.T) {
	h := setup(t)
	proof, err := h.engine.Store.PutArtifact("proof", "application/json", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	dump, err := h.engine.Store.PutArtifact("dataset", "application/vnd.envctl.postgres-dump", []byte("retained fixture bytes"))
	if err != nil {
		t.Fatal(err)
	}
	r := workflow.Result{Review: workflow.Review{EvidenceDigest: proof.Digest}, Datasets: map[string]workflow.DatasetSnapshot{"billing": {Source: dump, Evidence: proof}}}
	if err = h.engine.verify(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	d := r.Datasets["billing"]
	d.Source.Size++
	r.Datasets["billing"] = d
	if err = h.engine.verify(context.Background(), r); err == nil {
		t.Fatal("incorrect dump size accepted")
	}
	d.Source = dump
	d.Evidence.Digest = strings.Repeat("b", 64)
	r.Datasets["billing"] = d
	if err = h.engine.verify(context.Background(), r); err == nil {
		t.Fatal("missing dataset verification evidence accepted")
	}
}

func TestRewindDrainsHumanGateWithoutPublishing(t *testing.T) {
	h := setup(t)
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.State == "awaiting-approval" {
				return true
			}
		}
		return false
	})
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("plan", "", nil, h.now); return err })
	h.until(func(v *workflow.Revision) bool { return v.State == "superseded" })
	r := h.run()
	if h.backend.published != 0 {
		t.Fatal("superseded revision published")
	}
	cp := r.Revision(h.rev).Checkpoints["approved-change"]
	if !cp.HistoricalOnly || cp.Approval != nil {
		t.Fatal("drained output not archived as unapproved history")
	}
	if _, ok := r.Current().Checkpoints["approved-change"]; ok {
		t.Fatal("historical result leaked into new revision")
	}
}

func TestPluginAmendmentDrainsUnreadyPlanAsHistory(t *testing.T) {
	h := setup(t)
	h.backend.unready = true
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.Node == "plan" && a.Result != nil {
				return true
			}
		}
		return false
	})
	if _, ok := h.run().Current().Checkpoints["plan"]; ok {
		t.Fatal("unready Plan admitted")
	}
	h.mutate(func(r *workflow.Run) error {
		c := workflow.Clone(r.Current().Config)
		c.Plugins = append(c.Plugins, workflow.PluginRef{ID: "browser", Source: "./browser", Version: "1.0.0", Provides: []string{"browser.test"}})
		_, err := r.Rewind("plan", "", &c, h.now)
		return err
	})
	h.until(func(v *workflow.Revision) bool { return v.State == "superseded" })
	r := h.run()
	cp := r.Revision(h.rev).Checkpoints["plan"]
	if !cp.HistoricalOnly || !cp.Result.Review.Accepted {
		t.Fatal("old Plan lost reviewed history")
	}
	if _, ok := r.Current().Checkpoints["plan"]; ok {
		t.Fatal("unready historical Plan entered the new revision")
	}
	for _, a := range r.Revision(h.rev).Attempts {
		if a.Node != "task" && a.Node != "plan" {
			t.Fatal("old Plan dispatched downstream work")
		}
	}
	if err := r.Revision(h.rev).ValidateResult("plan", cp.Result, h.now); err == nil {
		t.Fatal("historical result can satisfy ordinary Plan admission")
	}
	if h.backend.published != 0 {
		t.Fatal("historical Plan published output")
	}
}

func TestRewindDuringPreparationNeverActivatesOldGuest(t *testing.T) {
	h := setup(t)
	h.backend.prepareHook = func() {
		h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("task", "", nil, h.now); return err })
	}
	h.step()
	v := h.run().Revision(h.rev)
	if v.State != "superseded" || !v.Runtime.Ready {
		t.Fatalf("ownership not retained for cleanup: %+v", v)
	}
	h.step()
	if h.backend.released != 1 || h.run().Revision(h.rev).Runtime.Ready {
		t.Fatal("superseded guest was not stopped")
	}
}

func TestDrainingFailureRetriesOnlyItsOriginalStage(t *testing.T) {
	h := setup(t)
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.Node == "code" && a.State == "running" {
				return true
			}
		}
		return false
	})
	v := h.run().Current()
	a := v.Attempts[len(v.Attempts)-1]
	h.backend.observations[a.ID] = Observation{State: "failed", Detail: "worker disconnected before completing implementation"}
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("plan", "", nil, h.now); return err })
	h.step()
	if h.run().Revision(h.rev).State != "draining" {
		t.Fatal("failed old worker was abandoned")
	}
	h.until(func(v *workflow.Revision) bool { return v.State == "superseded" })
	v = h.run().Revision(h.rev)
	if !v.Checkpoints["code"].HistoricalOnly {
		t.Fatal("retried work did not reach a historical checkpoint")
	}
	for _, a := range v.Attempts {
		if a.Node == "qa" {
			t.Fatal("draining dispatched a downstream stage")
		}
	}
}

func TestCapacityWaitAndRepeatedRewindSurviveSchedulerRecreation(t *testing.T) {
	h := setup(t)
	h.mutate(func(r *workflow.Run) error { r.Current().Config.Limits.VMs = 1; return nil })
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.Node == "code" && a.State == "running" {
				return true
			}
		}
		return false
	})
	old := h.run().Current()
	attempt := old.Attempts[len(old.Attempts)-1]
	h.backend.observations[attempt.ID] = Observation{State: "running"}
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("plan", "", nil, h.now); return err })
	queued := h.run().CurrentRevision
	tick := func() {
		t.Helper()
		h.now = h.now.Add(time.Second)
		if err := h.engine.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.engine.wg.Wait()
	}
	tick()
	if v := h.run().Revision(queued); v.State != "queued" || v.Runtime.ID != "" {
		t.Fatal("capacity wait allocated a second runtime")
	}
	// A new coordinator reads the queue from durable state; a second rewind
	// replaces that pending revision without interrupting the old live job.
	h.engine = &Engine{Store: h.engine.Store, Backend: h.backend, Now: h.backend.now}
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("task", "", nil, h.now); return err })
	latest := h.run().CurrentRevision
	tick()
	run := h.run()
	if run.Revision(queued).State != "superseded" || run.Revision(queued).Runtime.ID != "" || run.Current().State != "queued" || run.Revision(h.rev).State != "draining" {
		t.Fatal("repeated rewind lost queue or active drain ownership")
	}
	h.backend.observations[attempt.ID] = Observation{State: "completed", Result: h.backend.result(assign(run, run.Revision(h.rev), &attempt))}
	for i := 0; i < 12 && h.run().Revision(latest).State != "active"; i++ {
		tick()
	}
	run = h.run()
	if run.CurrentRevision != latest || run.Current().State != "active" || run.Revision(h.rev).Runtime.Ready || !run.Revision(h.rev).Checkpoints["code"].HistoricalOnly {
		t.Fatal("newest revision did not start after historical drain and release")
	}
	if run.Revision(queued).Runtime.ID != "" || h.backend.released != 1 || h.backend.published != 0 {
		t.Fatal("obsolete queue allocated resources or old work published")
	}
	for _, a := range run.Revision(h.rev).Attempts {
		if a.Node == "qa" {
			t.Fatal("draining revision advanced beyond its checkpoint")
		}
	}
}

func TestAttemptBudgetProducesVisibleInterventionState(t *testing.T) {
	h := setup(t)
	h.mutate(func(r *workflow.Run) error { r.Current().Config.Limits.MaxAttempts = 1; return nil })
	h.until(func(v *workflow.Revision) bool { return len(v.Attempts) > 0 })
	a := h.run().Current().Attempts[0]
	h.backend.observations[a.ID] = Observation{State: "failed", Detail: "fixture failure"}
	h.until(func(v *workflow.Revision) bool { return v.State == "needs-attention" })
	v := h.run().Current()
	if v.Recovery == nil || v.Recovery.Phase != "attempt-budget" {
		t.Fatal("budget exhaustion is silent")
	}
	if _, err := h.engine.Store.Artifact(v.Recovery.EvidenceDigest); err != nil {
		t.Fatal("budget exhaustion has no evidence")
	}
}

func TestParallelFailureDoesNotRestartItsSibling(t *testing.T) {
	h := setup(t)
	h.mutate(func(r *workflow.Run) error {
		c := &r.Current().Config
		c.Limits.Parallel = 2
		code := c.Workflow.Nodes["code"]
		delete(c.Workflow.Nodes, "code")
		c.Workflow.Nodes["left"], c.Workflow.Nodes["right"] = code, code
		merge := code
		merge.Needs = []string{"left", "right"}
		merge.Join = &workflow.JoinPolicy{Repositories: map[string]string{"app": "left"}}
		merge.Checks = []workflow.Check{{Name: "unit", Command: []string{"make", "test"}}}
		c.Workflow.Nodes["merge"] = merge
		qa := c.Workflow.Nodes["qa"]
		qa.Needs = []string{"merge"}
		c.Workflow.Nodes["qa"] = qa
		return c.Validate()
	})
	h.until(func(v *workflow.Revision) bool { return v.Active("left") && v.Active("right") })
	var left, right workflow.Attempt
	for _, a := range h.run().Current().Attempts {
		if a.Node == "left" {
			left = a
		}
		if a.Node == "right" {
			right = a
		}
	}
	h.backend.observations[left.ID] = Observation{State: "failed", Detail: "left fixture failed"}
	h.backend.observations[right.ID] = Observation{State: "running"}
	h.until(func(v *workflow.Revision) bool { _, ok := v.Checkpoints["left"]; return ok })
	if h.run().Current().Attempt(right.ID).State != "running" {
		t.Fatal("sibling changed during retry")
	}
	if h.run().Current().Active("merge") {
		t.Fatal("join dispatched before both parents completed")
	}
	run := h.run()
	h.backend.observations[right.ID] = Observation{State: "completed", Result: h.backend.result(assign(run, run.Current(), &right))}
	h.until(func(v *workflow.Revision) bool { return v.Active("merge") })
	v := h.run().Current()
	for _, a := range v.Attempts {
		if a.Node == "merge" && (a.Inputs["left"] != v.Checkpoints["left"].ID || a.Inputs["right"] != v.Checkpoints["right"].ID) {
			t.Fatal("join lost exact input checkpoint lineage")
		}
	}
}
