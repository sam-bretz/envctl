package workflow

import (
	"slices"
	"testing"
	"time"
)

func TestPlanRequirementCannotClaimReadinessAndSurvivesAmendment(t *testing.T) {
	r := live(t)
	v := r.Current()
	ready(v, time.Now())
	finish(t, r, "task")
	a, err := v.Begin("plan", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result := output(v, "plan")
	result.Requirements = []Requirement{{Capability: "billing.sandbox", Nodes: []string{"qa"}, Reason: "exercise sandbox checkout"}}
	review(&result)
	if err := v.Propose(a.ID, result, time.Now()); err == nil {
		t.Fatal("worker scope claim became readiness")
	}
	if err := v.RecordPlanResult("plan", result, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(v.Requirements(), "billing.sandbox") {
		t.Fatal("inventory not included")
	}
	ready(v, time.Now())
	if err := v.Propose(a.ID, result, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Accept(a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	old := v.ID
	if _, err := r.Rewind("plan", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(r.Current().Requirements(), "billing.sandbox") || len(r.Current().Readiness) == 0 && len(r.Current().ReadinessProblems(time.Now(), r.Current().Requirements())) == 0 {
		t.Fatal("amendment lost requirements or reused readiness")
	}
	if len(r.Revision(old).Checkpoints["plan"].Result.Requirements) != 1 {
		t.Fatal("historical inventory changed")
	}
	if _, err := r.Rewind("task", "A different objective", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(r.Current().Requirements(), "billing.sandbox") {
		t.Fatal("changed objective inherited old scope")
	}
}

func TestPlanInventoryValidationAndReviewBinding(t *testing.T) {
	v := live(t).Current()
	valid := Requirement{Capability: "billing.sandbox", Nodes: []string{"qa"}, Reason: "acceptance connection"}
	for _, requirements := range [][]Requirement{
		{{Capability: "bad / name", Nodes: []string{"qa"}, Reason: "x"}},
		{{Capability: "billing.sandbox", Nodes: []string{"unknown"}, Reason: "x"}},
		{{Capability: "billing.sandbox", Nodes: []string{"qa", "qa"}, Reason: "x"}},
		{{Capability: "billing.sandbox", Nodes: []string{}, Reason: "x"}},
		{{Capability: "billing.sandbox", Nodes: []string{"qa"}, Reason: " "}},
		{valid, valid},
	} {
		if err := v.RecordPlanRequirements("plan", requirements); err == nil {
			t.Fatal("invalid inventory admitted")
		}
	}
	r := output(v, "plan")
	r.Requirements = []Requirement{valid}
	if err := v.RecordPlanResult("plan", r, time.Now()); err == nil {
		t.Fatal("requirements changed after supervisor review")
	}
	review(&r)
	if err := v.RecordPlanResult("plan", r, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := v.RecordPlanRequirements("plan", nil); err != nil {
		t.Fatal(err)
	}
	if len(v.DiscoveredRequirements) != 1 {
		t.Fatal("retry silently removed known obligation")
	}
	if err := v.RecordPlanRequirements("code", []Requirement{valid}); err == nil {
		t.Fatal("non-Plan scope mutation accepted")
	}
}
