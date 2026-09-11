package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestProgressIsThrottledAndDeliveriesAreDurable(t *testing.T) {
	h := setup(t)
	h.backend.startRunning = true
	h.engine.ProgressInterval = 10 * time.Second
	var attempt string
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.State == "running" && h.backend.starts[a.ID] > 0 {
				attempt = a.ID
				return true
			}
		}
		return false
	})
	events := func() int {
		list, err := h.engine.Store.Events(context.Background(), h.id, 0, 1000)
		if err != nil {
			t.Fatal(err)
		}
		return len(list)
	}
	progress := func() *workflow.Progress {
		for _, a := range h.run().Current().Attempts {
			if a.ID == attempt {
				return a.Progress
			}
		}
		return nil
	}
	observe := func(o Observation) {
		t.Helper()
		o.State = "running"
		h.backend.observations[attempt] = o
		h.step()
	}
	observe(Observation{Progress: &workflow.Progress{Phase: "worker", Activity: []string{"agent: reading"}}})
	if p := progress(); p == nil || p.Phase != "worker" || p.UpdatedAt.IsZero() {
		t.Fatal("first progress not persisted", p)
	}
	base, version := events(), h.run().Version
	// Activity-only changes within the interval are observed but not written.
	for _, line := range []string{"ran (exit 0): ls", "ran (exit 1): pytest", "agent: fixing"} {
		observe(Observation{Progress: &workflow.Progress{Phase: "worker", Activity: []string{"agent: reading", line}}})
	}
	if len(progress().Activity) != 1 {
		t.Fatal("activity-only progress was not throttled")
	}
	// A phase change is written at once, outside the versioned run document:
	// progress never makes a client's version-fenced command stale.
	observe(Observation{Progress: &workflow.Progress{Phase: "checks", Detail: "check 1 of 1: unit"}})
	if p := progress(); p.Phase != "checks" || events() != base || h.run().Version != version {
		t.Fatal("phase change was throttled or changed the run version", p)
	}
	// Deliveries are durable immediately, once, even with unchanged progress.
	delivery := workflow.Delivery{Message: "msg_one", Role: "worker", Generation: 1, At: h.now}
	for i := 0; i < 3; i++ {
		observe(Observation{Progress: &workflow.Progress{Phase: "checks", Detail: "check 1 of 1: unit"}, Delivered: []workflow.Delivery{delivery}})
	}
	var steering []workflow.Delivery
	for _, a := range h.run().Current().Attempts {
		if a.ID == attempt {
			steering = a.Steering
		}
	}
	if len(steering) != 1 || steering[0].Message != "msg_one" || events() != base+1 {
		t.Fatal("delivery not recorded exactly once", steering, events()-base)
	}
	// After the interval, new activity is written again.
	h.now = h.now.Add(11 * time.Second)
	observe(Observation{Progress: &workflow.Progress{Phase: "checks", Detail: "check 1 of 1: unit", Activity: []string{"1 passed"}}})
	if p := progress(); len(p.Activity) != 1 || p.Activity[0] != "1 passed" {
		t.Fatal("progress not refreshed after the interval", p)
	}
	// A completed observation still records its late deliveries before the
	// attempt leaves the running state.
	late := workflow.Delivery{Message: "msg_two", Role: "supervisor", At: h.now}
	h.backend.observations[attempt] = Observation{State: "completed", Result: h.backend.result(assign(h.run(), h.run().Current(), &workflow.Attempt{ID: attempt, Node: "task"})), Delivered: []workflow.Delivery{delivery, late}}
	h.until(func(v *workflow.Revision) bool { return v.Attempt(attempt).State != "running" })
	if got := h.run().Current().Attempt(attempt).Steering; len(got) != 2 || got[1].Message != "msg_two" {
		t.Fatal("completion lost a delivery record", got)
	}
}
