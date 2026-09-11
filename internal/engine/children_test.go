package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

type childFixture struct {
	base        *fixtureBackend
	mu          sync.Mutex
	prepareWait map[string]<-chan struct{}
	entered     chan string
	missing     map[string]bool
	transport   map[string]bool
	releaseFail map[string]bool
	prepares    map[string]int
	dispatches  map[string][]Assignment
	releases    map[string]int
}

func (b *childFixture) IsolateBranches() bool { return true }
func (b *childFixture) Prepare(ctx context.Context, a Assignment) (Prepared, error) {
	if wait := b.prepareWait[a.Child]; wait != nil {
		if b.entered != nil {
			select {
			case b.entered <- a.Child:
			default:
			}
		}
		select {
		case <-wait:
		case <-ctx.Done():
			return Prepared{}, ctx.Err()
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.prepares[a.Revision.Runtime.ID]++
	return b.base.Prepare(ctx, a)
}
func (b *childFixture) Readiness(ctx context.Context, a Assignment) ([]workflow.Probe, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	probes, err := b.base.Readiness(ctx, a)
	if b.missing[a.Child] {
		for i := range probes {
			probes[i].Passed = false
			probes[i].Detail = "child connection unavailable"
		}
	}
	return probes, err
}
func (b *childFixture) Start(ctx context.Context, a Assignment) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dispatches[a.Child] = append(b.dispatches[a.Child], a)
	err := b.base.Start(ctx, a)
	if a.Child != "" {
		b.base.observations[a.Attempt.ID] = Observation{State: "running"}
	}
	return err
}
func (b *childFixture) Poll(ctx context.Context, a Assignment) (Observation, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.transport[a.Child] {
		return Observation{}, errors.New("fixture transport disconnected")
	}
	return b.base.Poll(ctx, a)
}
func (b *childFixture) Cancel(ctx context.Context, a Assignment) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base.Cancel(ctx, a)
}
func (b *childFixture) Release(_ context.Context, a Assignment) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.releaseFail[a.Child] {
		return errors.New("fixture release unavailable")
	}
	b.releases[a.Revision.Runtime.ID]++
	return nil
}
func (b *childFixture) Publish(ctx context.Context, a Assignment, r workflow.Result) (map[string]string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.base.Publish(ctx, a, r)
}
func (b *childFixture) finish(node string, failed bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	list := b.dispatches[node]
	a := list[len(list)-1]
	if failed {
		b.base.observations[a.Attempt.ID] = Observation{State: "failed", Detail: "fixture correction"}
	} else {
		result := b.base.result(a)
		artifact, _ := b.base.store.PutArtifact("source", "application/x-git-bundle", []byte("fixture retained source"))
		result.Sources = map[string]workflow.Artifact{"app": artifact}
		result.Review.ResultDigest = result.WorkDigest()
		b.base.observations[a.Attempt.ID] = Observation{State: "completed", Result: result}
	}
}
func (b *childFixture) count(node string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.dispatches[node])
}

func childSetup(t *testing.T, limit int) (*harness, *childFixture) {
	h := setup(t)
	h.mutate(func(r *workflow.Run) error {
		c := &r.Current().Config
		c.Limits.Parallel, c.Limits.VMs = 2, limit
		task, plan := c.Workflow.Nodes["task"], c.Workflow.Nodes["plan"]
		c.Workflow.Nodes = map[string]workflow.Node{"task": task, "plan": plan,
			"left":  {Kind: "custom", Needs: []string{"plan"}, Outputs: []string{"left"}, Priority: 10},
			"right": {Kind: "custom", Needs: []string{"plan"}, Outputs: []string{"right"}},
		}
		return c.Validate()
	})
	b := &childFixture{base: h.backend, prepareWait: map[string]<-chan struct{}{}, missing: map[string]bool{}, transport: map[string]bool{}, releaseFail: map[string]bool{}, prepares: map[string]int{}, dispatches: map[string][]Assignment{}, releases: map[string]int{}}
	h.engine.Backend = b
	return h, b
}
func childStep(h *harness) {
	h.t.Helper()
	h.step()
	nodes := []string{}
	for node := range h.run().Revision(h.rev).ChildRuntimes {
		nodes = append(nodes, node)
	}
	slices.Sort(nodes)
	for _, node := range nodes {
		if err := h.engine.ReconcileChild(context.Background(), h.id, h.rev, node); err != nil && !errors.Is(err, workflow.ErrConflict) {
			h.t.Fatal(err)
		}
	}
}
func childUntil(h *harness, fn func() bool) {
	h.t.Helper()
	for i := 0; i < 150; i++ {
		if fn() {
			return
		}
		childStep(h)
	}
	h.t.Fatalf("child condition not reached: %+v", h.run().Revision(h.rev))
}

func TestChildCapacityPriorityRetryAndRelease(t *testing.T) {
	h, b := childSetup(t, 2)
	childUntil(h, func() bool { return b.count("left") == 1 })
	first := h.run().Current().ChildRuntimes["left"].Runtime.ID
	if h.run().VMCount() != 2 || b.count("right") != 0 || h.run().Current().ChildRuntimes["right"] != nil {
		t.Fatal("VM capacity/priority not enforced at reservation")
	}
	b.finish("left", true)
	// A restart reads and reuses the failed branch VM/session namespace.
	h.engine = &Engine{Store: h.engine.Store, Backend: b, Now: b.base.now}
	childUntil(h, func() bool { return b.count("left") == 2 })
	if h.run().Current().ChildRuntimes["left"].Runtime.ID != first || b.count("right") != 0 {
		t.Fatal("retry consumed a different VM or dispatched sibling over budget")
	}
	b.releaseFail["left"] = true
	b.finish("left", false)
	childUntil(h, func() bool { return h.run().Current().ChildRuntimes["left"].Recovery != nil })
	if h.run().VMCount() != 2 || b.count("right") != 0 {
		t.Fatal("failed release freed capacity prematurely")
	}
	b.releaseFail["left"] = false
	childUntil(h, func() bool { return b.count("right") == 1 })
	v := h.run().Current()
	if v.ChildRuntimes["left"].Runtime.OccupiesVM() || v.ChildRuntimes["right"].Runtime.ID == first || h.run().VMCount() != 2 {
		t.Fatal("checkpoint release did not transfer capacity to a distinct child")
	}
	b.finish("right", false)
	childUntil(h, func() bool { return h.run().Current().State == "completed" })
	if h.run().VMCount() != 2 || b.releases[first] != 1 || !h.run().Current().ChildRuntimes["right"].Runtime.Ready {
		t.Fatal("completed run lost its application runtime or replayed a completed release")
	}
	h.mutate(func(r *workflow.Run) error { r.Cancel(h.now); return nil })
	childUntil(h, func() bool { return h.run().VMCount() == 0 })
}

func TestChildReadinessAndRecoveryDoNotStopSibling(t *testing.T) {
	h, b := childSetup(t, 3)
	b.missing["left"] = true
	childUntil(h, func() bool { return b.count("right") == 1 })
	if b.count("left") != 0 {
		t.Fatal("parent readiness authorized an unprepared child")
	}
	b.missing["left"] = false
	childUntil(h, func() bool { return b.count("left") == 1 })
	b.transport["left"] = true
	b.finish("right", false)
	childUntil(h, func() bool { _, ok := h.run().Current().Checkpoints["right"]; return ok })
	v := h.run().Current()
	if v.Recovery != nil || v.ChildRuntimes["left"].Recovery == nil {
		t.Fatal("branch reconnect failure escaped its recovery scope")
	}
	if _, err := h.engine.Store.Artifact(v.ChildRuntimes["left"].Recovery.EvidenceDigest); err != nil {
		t.Fatal("child recovery has no retained evidence")
	}
	b.transport["left"] = false
	b.finish("left", false)
	childUntil(h, func() bool { return h.run().Current().State == "completed" })
}

func TestSlowChildProvisioningDoesNotHoldSiblingLock(t *testing.T) {
	h, b := childSetup(t, 3)
	// Prepare parent/Plan and reserve children without starting their work.
	h.until(func(v *workflow.Revision) bool { return len(v.ChildRuntimes) == 2 })
	release := make(chan struct{})
	b.prepareWait["left"] = release
	b.entered = make(chan string, 1)
	// Concurrent Tick uses a stable wall clock, not the serial fixture's clock.
	b.base.now = time.Now
	h.engine.Now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { close(release); cancel(); h.engine.wg.Wait() }()
	if err := h.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		t.Fatal("left did not enter provisioning")
	}
	deadline := time.Now().Add(3 * time.Second)
	for b.count("right") == 0 && time.Now().Before(deadline) {
		if err := h.engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if b.count("right") != 1 || b.count("left") != 0 {
		t.Fatal("slow child provisioning starved its sibling")
	}
	for i := 0; i < 4; i++ {
		if err := h.engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if b.count("right") != 1 {
		t.Fatal("parallel ticks duplicated dispatch")
	}
}

func TestChildRewindCountsDrainingOwnership(t *testing.T) {
	h, b := childSetup(t, 3)
	childUntil(h, func() bool { return b.count("left") == 1 && b.count("right") == 1 })
	old := h.rev
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("plan", "", nil, h.now); return err })
	latest := h.run().CurrentRevision
	tick := func() {
		t.Helper()
		h.now = h.now.Add(time.Second)
		if err := h.engine.Tick(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.engine.wg.Wait()
	}
	tick()
	if h.run().Current().State != "queued" || h.run().Current().Runtime.ID != "" || h.run().VMCount() != 3 {
		t.Fatal("rewind allocated over draining child capacity")
	}
	b.finish("left", false)
	for i := 0; i < 50 && h.run().Current().State == "queued"; i++ {
		tick()
	}
	run := h.run()
	if run.CurrentRevision != latest || run.Current().State == "queued" || run.VMCount() > 3 || !run.Revision(old).Checkpoints["left"].HistoricalOnly {
		t.Fatal("new revision did not use released child capacity with historical fencing")
	}
	if run.Revision(old).State != "draining" || run.Revision(old).ChildRuntimes["right"].Runtime.Ready == false {
		t.Fatal("rewind stopped unfinished sibling")
	}
	if len(run.Current().ChildRuntimes) != 0 {
		t.Fatal("rewind copied old child ownership")
	}
	b.finish("right", false)
	for i := 0; i < 80 && (h.run().Revision(old).Runtime.Ready || h.run().Revision(old).HasChildVMs()); i++ {
		tick()
		if h.run().VMCount() > 3 {
			t.Fatal("overlapping revisions exceeded VM capacity")
		}
	}
	run = h.run()
	if run.Revision(old).Runtime.Ready || run.Revision(old).HasChildVMs() || !run.Revision(old).Checkpoints["right"].HistoricalOnly {
		t.Fatal("old child ownership did not drain and release")
	}
}

func TestCancelDuringChildProvisioningRetainsReleaseOwnership(t *testing.T) {
	h, b := childSetup(t, 3)
	h.until(func(v *workflow.Revision) bool { return len(v.ChildRuntimes) == 2 })
	gate := make(chan struct{})
	b.prepareWait["left"], b.prepareWait["right"] = gate, gate
	b.entered = make(chan string, 2)
	b.base.now = time.Now
	h.engine.Now = time.Now
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.entered:
	case <-time.After(time.Second):
		close(gate)
		h.engine.wg.Wait()
		t.Fatal("provisioning did not start")
	}
	h.mutate(func(r *workflow.Run) error { r.Cancel(time.Now()); return nil })
	if err := h.engine.Reconcile(ctx, h.id, h.rev); err != nil {
		t.Fatal(err)
	}
	if h.run().VMCount() != 3 {
		t.Fatal("cancellation dropped unresolved VM reservations")
	}
	close(gate)
	h.engine.wg.Wait()
	for i := 0; i < 30 && h.run().VMCount() > 0; i++ {
		if err := h.engine.Tick(ctx); err != nil {
			t.Fatal(err)
		}
		h.engine.wg.Wait()
	}
	if h.run().VMCount() != 0 || b.count("left") != 0 || b.count("right") != 0 {
		t.Fatal("cancelled provisioning dispatched a worker or leaked a VM")
	}
}

type aliasedChildFixture struct {
	*childFixture
	daemon string
	mutate func(*Prepared)
}

func (b *aliasedChildFixture) Prepare(ctx context.Context, a Assignment) (Prepared, error) {
	p, err := b.childFixture.Prepare(ctx, a)
	if a.Child != "" {
		if b.daemon != "" {
			p.Runtime.DaemonID = b.daemon
		}
		if b.mutate != nil {
			b.mutate(&p)
		}
	}
	return p, err
}

func TestChildPreparationRejectsSourceAndImageDrift(t *testing.T) {
	image := "sha256:" + strings.Repeat("c", 64)
	for _, kind := range []string{"source", "image"} {
		t.Run(kind, func(t *testing.T) {
			h, b := childSetup(t, 3)
			h.until(func(v *workflow.Revision) bool { return len(v.ChildRuntimes) == 2 })
			h.mutate(func(r *workflow.Run) error { r.Current().Runtime.ImageDigest = image; return nil })
			h.engine.Backend = &aliasedChildFixture{childFixture: b, mutate: func(p *Prepared) {
				p.Runtime.ImageDigest = image
				if kind == "source" {
					p.SourcePins["app"] = strings.Repeat("f", 40)
				} else {
					p.Runtime.ImageDigest = "sha256:" + strings.Repeat("d", 64)
				}
			}}
			if err := h.engine.ReconcileChild(context.Background(), h.id, h.rev, "left"); err != nil {
				t.Fatal(err)
			}
			child := h.run().Current().ChildRuntimes["left"]
			if child.Runtime.Ready || child.Recovery == nil || b.count("left") != 0 {
				t.Fatal("child admitted with changed immutable input", kind)
			}
		})
	}
}

func TestChildCannotUseParentDockerIdentity(t *testing.T) {
	h, b := childSetup(t, 3)
	h.until(func(v *workflow.Revision) bool { return len(v.ChildRuntimes) == 2 })
	h.engine.Backend = &aliasedChildFixture{childFixture: b, daemon: h.run().Current().Runtime.DaemonID}
	if err := h.engine.ReconcileChild(context.Background(), h.id, h.rev, "left"); err != nil {
		t.Fatal(err)
	}
	child := h.run().Current().ChildRuntimes["left"]
	if child.Runtime.Ready || child.Recovery == nil || b.count("left") != 0 {
		t.Fatal("shared Docker daemon admitted as isolated branch")
	}
}
