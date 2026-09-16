package engine

import (
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func (h *harness) ticksUntil(what string, done func(*workflow.Run) bool) {
	h.t.Helper()
	for i := 0; i < 600; i++ {
		r := h.run()
		if done(r) {
			return
		}
		// limits.vms bounds the VMs a run holds, variations included.
		if n := r.VMCount(); n > r.Current().Config.Limits.VMs {
			h.t.Fatalf("the run holds %d VMs, over its limit of %d", n, r.Current().Config.Limits.VMs)
		}
		h.tick()
	}
	h.t.Fatalf("never reached: %s", what)
}

func TestVariationsAreBuiltSideBySideAndOnlyTheChosenOnePublishes(t *testing.T) {
	h := setupWorkflow(t, "{template: feature, nodes: {design: {variations: 3}}}")
	h.backend.variations = map[string][]workflow.Variation{"design": {
		{Name: "polling", Rationale: "if the upstream has no webhooks"},
		{Name: "streaming", Rationale: "at high volume"},
	}}

	// Branching: the accepted design becomes three candidates.
	h.ticksUntil("the design stage branches", func(r *workflow.Run) bool { return len(r.Revisions) == 3 })
	r := h.run()
	groups := r.VariationGroups()
	if len(groups) != 1 {
		t.Fatalf("want one comparison, got %v", groups)
	}

	// Every candidate is built through QA. The default limit of two VMs is
	// fewer than three candidates, so this only finishes if a finished one
	// gives its VM to the one still waiting.
	h.ticksUntil("every variation finishes its stages", func(r *workflow.Run) bool {
		for _, v := range r.Variations(groups[0]) {
			if !v.Finished() {
				return false
			}
		}
		return true
	})
	r = h.run()
	for _, v := range r.Variations(groups[0]) {
		// A candidate that proposed an alternative really built its own design.
		if v.Variant.Name != workflow.AsProposed && v.Attempt(v.Checkpoints["design"].Attempt) == nil {
			t.Fatalf("%s reused another variation's design instead of building its own", v.Variant.Name)
		}
		if _, ok := v.Checkpoints["approved-change"]; ok {
			t.Fatalf("%s reached its approved change before anyone chose", v.Variant.Name)
		}
		for _, a := range v.Attempts {
			if a.Node == "approved-change" {
				t.Fatalf("%s started its approved change before anyone chose", v.Variant.Name)
			}
		}
	}
	if h.backend.published != 0 {
		t.Fatal("a variation published before anyone chose")
	}
	if h.backend.released == 0 {
		t.Fatal("no finished variation gave up its VM, so the comparison could not have fit the limit")
	}

	// Parking is precise: a finished variation gives up its VM only while a
	// sibling is still queued for one. The first to finish parks; the ones
	// that finish after the last sibling started keep theirs.
	var streaming string
	parked := 0
	for _, v := range r.Variations(groups[0]) {
		if v.Parked() {
			parked++
			streaming = v.ID
		}
	}
	if parked != 1 {
		t.Fatalf("want exactly the first finisher parked, got %d", parked)
	}
	// Choose the parked one, so choosing has to give it a VM again and
	// restore its source before its approved change can run.
	h.mutate(func(r *workflow.Run) error { return r.Choose(streaming, h.now) })
	if r := h.run(); r.Revision(streaming).State != "queued" {
		t.Fatalf("a chosen parked variation was not queued for a VM: %s", r.Revision(streaming).State)
	}
	h.rev = streaming

	h.ticksUntil("the chosen variation waits for approval", func(r *workflow.Run) bool {
		for _, a := range r.Revision(streaming).Attempts {
			if a.Node == "approved-change" && a.State == "awaiting-approval" {
				return true
			}
		}
		return false
	})
	r = h.run()
	var waiting workflow.Attempt
	for _, a := range r.Revision(streaming).Attempts {
		if a.State == "awaiting-approval" {
			waiting = a
		}
	}
	h.mutate(func(r *workflow.Run) error {
		return r.Approve(streaming, waiting.ID, "developer", waiting.Result.WorkDigest(), h.now)
	})
	h.ticksUntil("the chosen variation completes", func(r *workflow.Run) bool { return r.Revision(streaming).State == "completed" })
	for i := 0; i < 5; i++ {
		h.tick()
	}

	r = h.run()
	if h.backend.published != 1 || r.CurrentRevision != streaming {
		t.Fatalf("want exactly the chosen variation published: %d publications, current %s", h.backend.published, r.CurrentRevision)
	}
	for _, v := range r.Variations(groups[0]) {
		if v.ID == streaming {
			continue
		}
		if v.State != workflow.NotChosen || v.Runtime.Ready || len(v.Checkpoints) == 0 {
			t.Fatalf("%s was not retired with its VM released and evidence kept: %s ready=%v", v.Variant.Name, v.State, v.Runtime.Ready)
		}
	}
}
