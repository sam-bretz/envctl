package engine

import (
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestTokenCeilingStopsAgentsAndBlocksNewWork(t *testing.T) {
	h := setup(t)
	h.backend.startRunning = true
	h.mutate(func(r *workflow.Run) error {
		r.Current().Config.Limits.RunTokens = 1000
		return nil
	})
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
	running := func(u workflow.Usage) {
		h.backend.observations[attempt] = Observation{State: "running", Progress: &workflow.Progress{Phase: "worker"}, Usage: &u}
		h.step()
	}
	running(workflow.Usage{Input: 100, CacheRead: 300, Output: 100, CostUSD: 0.02, Estimated: true})
	if got := h.run().Usage(); got.Tokens() != 500 || !got.Estimated {
		t.Fatalf("live usage not visible: %+v", got)
	}
	if over, _ := h.run().Budget(); over {
		t.Fatal("under the ceiling reported as exceeded")
	}

	// Crossing the ceiling cancels the running agent. The fixture's cancelled
	// job reports no usage, which must not erase what the attempt used.
	running(workflow.Usage{Input: 200, CacheRead: 700, Output: 300, Estimated: true})
	h.until(func(v *workflow.Revision) bool { return v.Attempt(attempt).State == "failed" })
	a := h.run().Current().Attempt(attempt)
	if a.Usage == nil || a.Usage.Tokens() != 1200 || a.Usage.Estimated {
		t.Fatalf("stopped attempt lost its usage: %+v", a.Usage)
	}
	if over, reason := h.run().Budget(); !over || !strings.Contains(reason, "limits.run_tokens") {
		t.Fatalf("ceiling not reported: %v %q", over, reason)
	}

	// The retry becomes ready but is never started.
	starts := len(h.backend.starts)
	h.until(func(v *workflow.Revision) bool { return v.State == "needs-attention" })
	v := h.run().Current()
	if len(h.backend.starts) != starts || h.backend.starts[attempt] != 1 {
		t.Fatal("work was dispatched past the token ceiling")
	}
	if v.Recovery == nil || v.Recovery.Phase != "token-budget" || !strings.Contains(v.Recovery.Detail, "run rewind --config") {
		t.Fatalf("needs-attention does not explain the ceiling: %+v", v.Recovery)
	}
}

func TestProbeUsageCountsTowardTheRun(t *testing.T) {
	h := setup(t)
	h.mutate(func(r *workflow.Run) error {
		r.Current().Config.Limits.RunTokens = -1 // no ceiling
		return nil
	})
	h.backend.probeUsage = &workflow.Usage{CacheWrite: 1000, Output: 50, CostUSD: 0.01}
	h.until(func(v *workflow.Revision) bool { return len(v.ProbeUsage) > 0 })
	u := h.run().Usage()
	if u.Tokens() < 1050 || u.CostUSD < 0.01 {
		t.Fatalf("probe usage not counted: %+v", u)
	}
	if !strings.Contains(h.run().UsageSummary(), "no ceiling") {
		t.Fatalf("disabled ceiling not shown: %q", h.run().UsageSummary())
	}
}
