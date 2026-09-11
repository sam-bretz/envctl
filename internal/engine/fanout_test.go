package engine

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// serialBackend lets Tick reconcile overlapping revisions concurrently while the
// map-based fixture observes one call at a time.
type serialBackend struct {
	mu sync.Mutex
	b  *fixtureBackend
}

func (s *serialBackend) Prepare(ctx context.Context, a Assignment) (Prepared, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Prepare(ctx, a)
}
func (s *serialBackend) Readiness(ctx context.Context, a Assignment) ([]workflow.Probe, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Readiness(ctx, a)
}
func (s *serialBackend) Start(ctx context.Context, a Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Start(ctx, a)
}
func (s *serialBackend) Poll(ctx context.Context, a Assignment) (Observation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Poll(ctx, a)
}
func (s *serialBackend) Cancel(ctx context.Context, a Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Cancel(ctx, a)
}
func (s *serialBackend) Release(ctx context.Context, a Assignment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Release(ctx, a)
}
func (s *serialBackend) Publish(ctx context.Context, a Assignment, r workflow.Result) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Publish(ctx, a, r)
}

// fanoutSetup builds Task -> Plan -> Design -> {left, right} -> merge -> QA ->
// approved change, with both branches runnable at once.
func fanoutSetup(t *testing.T, edit func(*workflow.Config)) *harness {
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
		if edit != nil {
			edit(c)
		}
		return c.Validate()
	})
	h.engine.Backend = &serialBackend{b: h.backend}
	return h
}
func (h *harness) tick() {
	h.t.Helper()
	h.now = h.now.Add(time.Second)
	if err := h.engine.Tick(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	h.engine.wg.Wait()
}
func (h *harness) tickUntil(what string, fn func(*workflow.Run) bool) {
	h.t.Helper()
	for i := 0; i < 150; i++ {
		if fn(h.run()) {
			return
		}
		h.tick()
	}
	h.t.Fatalf("%s never happened: %+v", what, h.run().Current())
}
func attemptsFor(v *workflow.Revision, node string) []workflow.Attempt {
	var out []workflow.Attempt
	for _, a := range v.Attempts {
		if a.Node == node {
			out = append(out, a)
		}
	}
	return out
}
func first(v *workflow.Revision, node string) (workflow.Attempt, bool) {
	list := attemptsFor(v, node)
	if len(list) == 0 {
		return workflow.Attempt{}, false
	}
	return list[0], true
}

func TestFanoutRewindToBranchReusesSiblingAndInvalidatesJoin(t *testing.T) {
	h := fanoutSetup(t, nil)
	var merge workflow.Attempt
	h.until(func(v *workflow.Revision) bool { var ok bool; merge, ok = first(v, "merge"); return ok })
	// Hold the join in flight so the old revision must drain it.
	h.backend.observations[merge.ID] = Observation{State: "running"}
	before := h.run().Current()
	old := h.rev
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("left", "", nil, h.now); return err })
	run := h.run()
	next := run.Current()
	for _, kept := range []string{"task", "plan", "design", "right"} {
		if next.Checkpoints[kept].ID == "" || next.Checkpoints[kept].ID != before.Checkpoints[kept].ID {
			t.Fatalf("unaffected checkpoint %s was not reused", kept)
		}
	}
	for _, gone := range []string{"left", "merge", "qa", "approved-change"} {
		if _, ok := next.Checkpoints[gone]; ok {
			t.Fatalf("rewound or downstream checkpoint %s survived", gone)
		}
	}
	if o := run.Revision(old); o.State != "draining" || !slices.Equal(o.DrainNodes, []string{"merge"}) {
		t.Fatalf("old revision must drain only its in-flight join: %s %v", o.State, o.DrainNodes)
	}
	h.tickUntil("replacement join", func(r *workflow.Run) bool { _, ok := first(r.Current(), "merge"); return ok })
	run = h.run()
	cur := run.Current()
	if len(attemptsFor(cur, "right")) != 0 || len(attemptsFor(cur, "left")) != 1 {
		t.Fatal("rewind re-executed the reused sibling or skipped the rewound branch")
	}
	replacement, _ := first(cur, "merge")
	if replacement.Inputs["right"] != before.Checkpoints["right"].ID || replacement.Inputs["left"] != cur.Checkpoints["left"].ID || replacement.Inputs["left"] == before.Checkpoints["left"].ID {
		t.Fatalf("join did not bind the reused sibling and the new branch: %v", replacement.Inputs)
	}
	o := run.Revision(old)
	if o.Attempt(merge.ID).State != "running" || len(attemptsFor(o, "qa")) != 0 {
		t.Fatal("draining join was interrupted or its revision advanced past the join")
	}
}

func TestFanoutRewindDrainsInFlightBranchAsHistory(t *testing.T) {
	h := fanoutSetup(t, nil)
	var left workflow.Attempt
	h.until(func(v *workflow.Revision) bool { var ok bool; left, ok = first(v, "left"); return ok })
	h.backend.observations[left.ID] = Observation{State: "running"}
	h.until(func(v *workflow.Revision) bool { _, ok := v.Checkpoints["right"]; return ok })
	rightCP := h.run().Current().Checkpoints["right"].ID
	old := h.rev
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("left", "", nil, h.now); return err })
	if o := h.run().Revision(old); o.State != "draining" || !slices.Equal(o.DrainNodes, []string{"left"}) {
		t.Fatalf("in-flight branch was not drained: %s %v", o.State, o.DrainNodes)
	}
	h.tickUntil("replacement branch", func(r *workflow.Run) bool { _, ok := first(r.Current(), "left"); return ok })
	run := h.run()
	if run.Revision(old).Attempt(left.ID).State != "running" {
		t.Fatal("rewind stopped the old in-flight branch instead of draining it")
	}
	if replacement, _ := first(run.Current(), "left"); replacement.ID == left.ID {
		t.Fatal("replacement reused the superseded attempt")
	}
	h.backend.observations[left.ID] = Observation{State: "completed", Result: h.backend.result(assign(run, run.Revision(old), &left))}
	h.tickUntil("old revision drain", func(r *workflow.Run) bool { return r.Revision(old).State == "superseded" })
	h.tickUntil("replacement join", func(r *workflow.Run) bool { _, ok := first(r.Current(), "merge"); return ok })
	run = h.run()
	o, cur := run.Revision(old), run.Current()
	if !o.Checkpoints["left"].HistoricalOnly || len(attemptsFor(o, "merge")) != 0 {
		t.Fatal("drained branch was not archived as history, or its revision dispatched the join")
	}
	if cur.Checkpoints["left"].ID == o.Checkpoints["left"].ID || cur.Checkpoints["right"].ID != rightCP || len(attemptsFor(cur, "right")) != 0 {
		t.Fatal("historical output leaked into the replacement, or the sibling was not reused")
	}
	join, _ := first(cur, "merge")
	if join.Inputs["left"] != cur.Checkpoints["left"].ID || join.Inputs["right"] != rightCP {
		t.Fatalf("join lineage is wrong: %v", join.Inputs)
	}
}

func TestFanoutRewindToPlanInvalidatesEveryBranch(t *testing.T) {
	h := fanoutSetup(t, nil)
	var merge workflow.Attempt
	h.until(func(v *workflow.Revision) bool { var ok bool; merge, ok = first(v, "merge"); return ok })
	h.backend.observations[merge.ID] = Observation{State: "running"}
	task := h.run().Current().Checkpoints["task"].ID
	h.mutate(func(r *workflow.Run) error { _, err := r.Rewind("plan", "", nil, h.now); return err })
	next := h.run().Current()
	if len(next.Checkpoints) != 1 || next.Checkpoints["task"].ID != task {
		t.Fatalf("Plan rewind kept work below Plan: %v", next.Checkpoints)
	}
	h.tickUntil("both branches re-executed", func(r *workflow.Run) bool {
		_, l := first(r.Current(), "left")
		_, rt := first(r.Current(), "right")
		return l && rt
	})
}

func TestNodeAttemptBudgetExhaustsOnlyThatBranch(t *testing.T) {
	h := fanoutSetup(t, func(c *workflow.Config) {
		left := c.Workflow.Nodes["left"]
		left.Limits.MaxAttempts = 1
		c.Workflow.Nodes["left"] = left
	})
	var left, right workflow.Attempt
	h.until(func(v *workflow.Revision) bool {
		var l, r bool
		left, l = first(v, "left")
		right, r = first(v, "right")
		return l && r
	})
	h.backend.observations[left.ID] = Observation{State: "failed", Detail: "left fixture failure"}
	h.backend.observations[right.ID] = Observation{State: "failed", Detail: "right fixture failure"}
	h.until(func(v *workflow.Revision) bool {
		_, ok := v.Checkpoints["right"]
		return ok || v.State == "needs-attention"
	})
	v := h.run().Current()
	if _, ok := v.Checkpoints["right"]; !ok || len(attemptsFor(v, "right")) != 2 {
		t.Fatalf("exhausting left stopped its sibling's retry: state %s, right attempts %d", v.State, len(attemptsFor(v, "right")))
	}
	h.until(func(v *workflow.Revision) bool { return v.State == "needs-attention" })
	v = h.run().Current()
	if len(attemptsFor(v, "left")) != 1 {
		t.Fatal("left exceeded its node budget")
	}
	if v.Recovery == nil || v.Recovery.Phase != "attempt-budget" || !strings.Contains(v.Recovery.Detail, "stage left exhausted its configured 1 attempts") {
		t.Fatalf("budget exhaustion does not name the node: %+v", v.Recovery)
	}
}
