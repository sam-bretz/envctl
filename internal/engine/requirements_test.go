package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestDiscoveredPlanRequirementHoldsAcrossRestartUntilProbePasses(t *testing.T) {
	h := setup(t)
	h.backend.planRequirements = []workflow.Requirement{{Capability: "billing.sandbox", Nodes: []string{"qa"}, Reason: "End-to-end acceptance needs an isolated billing tenant"}}
	h.backend.missing = map[string]bool{"billing.sandbox": true}
	h.until(func(v *workflow.Revision) bool { return len(v.DiscoveredRequirements) == 1 })
	before := h.run().Current()
	if !strings.Contains(strings.Join(before.ReadinessProblems(h.now, before.Requirements()), "\n"), "billing.sandbox") {
		t.Fatal("discovered capability was not an admission obligation")
	}
	h.engine = &Engine{Store: h.engine.Store, Backend: h.backend, Now: h.backend.now}
	for i := 0; i < 40; i++ {
		h.step()
	}
	v := h.run().Current()
	if _, ok := v.Checkpoints["plan"]; ok {
		t.Fatal("missing discovered capability admitted Plan")
	}
	for _, attempt := range v.Attempts {
		if attempt.Node != "task" && attempt.Node != "plan" {
			t.Fatal("downstream node dispatched before discovery was resolved")
		}
		if attempt.Node == "plan" && (attempt.Result == nil || len(attempt.Result.Requirements) != 1 || h.backend.starts[attempt.ID] != 1) {
			t.Fatal("readiness wait discarded Plan work or reran its worker")
		}
	}
	h.backend.missing["billing.sandbox"] = false
	h.now = h.now.Add(time.Minute)
	h.until(func(v *workflow.Revision) bool { _, ok := v.Checkpoints["design"]; return ok })
	if len(h.run().Current().Checkpoints["plan"].Result.Requirements) != 1 {
		t.Fatal("accepted Plan lost its reviewed inventory")
	}
}
