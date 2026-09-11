package engine

import (
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// A stall is display state while the backend intervenes, then an ordinary
// attempt failure: the next attempt follows the existing retry budget.
func TestStallIsVisibleThenRetriedUnderTheAttemptBudget(t *testing.T) {
	h := setup(t)
	h.backend.startRunning = true
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
	node := h.run().Current().Attempt(attempt).Node
	h.backend.observations[attempt] = Observation{State: "running", Progress: &workflow.Progress{Phase: "worker", Detail: "no agent output for 6m (intervenes at 10m)"}}
	h.step()
	if p := h.run().Current().Attempt(attempt).Progress; p == nil || !strings.Contains(p.Detail, "no agent output") {
		t.Fatal("stall watch not visible to clients", p)
	}
	h.backend.observations[attempt] = Observation{State: "running", Progress: &workflow.Progress{Phase: "worker", Detail: "stalled; stopping the agent: worker produced no output for 11m"}}
	h.step()
	if p := h.run().Current().Attempt(attempt).Progress; p == nil || !strings.HasPrefix(p.Detail, "stalled;") {
		t.Fatal("stall decision not visible before the attempt fails", p)
	}
	h.backend.observations[attempt] = Observation{State: "failed", Detail: "stalled: worker produced no output for 11m (stall window 10m); no response to the coordinator's stall nudge"}
	h.until(func(v *workflow.Revision) bool {
		retried := 0
		for _, a := range v.Attempts {
			if a.Node == node {
				retried++
			}
		}
		return v.Attempt(attempt).State == "failed" && retried > 1
	})
	if got := h.run().Current().Attempt(attempt); !strings.Contains(got.Error, "no response to the coordinator's stall nudge") {
		t.Fatal("stall evidence lost from the failed attempt", got.Error)
	}
}
