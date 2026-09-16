package workflow

import (
	"strings"
	"testing"
	"time"
)

// variationRun is a run whose design stage allows three variations and has
// been accepted, with task and plan checkpointed before it.
func variationRun(t *testing.T) *Run {
	t.Helper()
	c, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature, nodes: {design: {variations: 3}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRun("demo", "ship it", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rev := r.Current()
	rev.State = "active"
	rev.Runtime = RuntimeState{ID: "vm", Ready: true}
	for _, node := range []string{"task", "plan", "design"} {
		rev.Checkpoints[node] = Checkpoint{ID: "cp_" + node, Node: node, Result: Result{Summary: node + " done", Review: Review{Accepted: true, Summary: node + " reviewed"}}}
	}
	return r
}

var proposals = []Variation{{Name: "polling", Rationale: "if the upstream has no webhooks"}, {Name: "streaming", Rationale: "at high volume"}}

func TestBranchingBuildsEachAlternativeBesideTheApproachTaken(t *testing.T) {
	r := variationRun(t)
	current := r.CurrentRevision
	if err := r.Branch(current, "design", proposals, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.CurrentRevision != current {
		t.Fatal("branching changed the current revision before anyone chose")
	}
	if len(r.Revisions) != 3 {
		t.Fatalf("want the original plus 2 siblings, got %d revisions", len(r.Revisions))
	}
	source := r.Revision(current)
	if source.Variant == nil || source.Variant.Name != AsProposed || source.State != "active" {
		t.Fatalf("the approach taken does not continue as a variation: %+v", source.Variant)
	}
	for _, sibling := range r.Revisions[1:] {
		if sibling.Variant == nil || sibling.Variant.Group != source.Variant.Group || sibling.State != "queued" || sibling.Parent != current {
			t.Fatalf("sibling not in the comparison: %+v", sibling)
		}
		// Isolation: the varied stage and what follows it rerun; what came
		// before is reused.
		if _, ok := sibling.Checkpoints["plan"]; !ok {
			t.Fatal("a sibling reruns a stage before the one being varied")
		}
		for _, rerun := range []string{"design", "code", "qa", "approved-change"} {
			if _, ok := sibling.Checkpoints[rerun]; ok {
				t.Fatalf("a sibling kept %s, which it must build its own way", rerun)
			}
		}
		last := sibling.Messages[len(sibling.Messages)-1]
		if last.Node != "design" || last.Recipient != "worker" || !strings.Contains(last.Body, sibling.Variant.Name) || !strings.Contains(last.Body, sibling.Variant.Rationale) {
			t.Fatalf("the worker is not told which alternative to build: %+v", last)
		}
	}
	for _, rev := range r.Revisions {
		if rev.State == "superseded" {
			t.Fatal("branching superseded a revision that is still a candidate")
		}
	}
}

func TestBranchingIsRefusedWhenItWouldNotBeACleanComparison(t *testing.T) {
	cases := map[string]func(r *Run) error{
		"a stage that did not opt in": func(r *Run) error { return r.Branch(r.CurrentRevision, "plan", proposals, time.Now()) },
		"a lone alternative":          func(r *Run) error { return r.Branch(r.CurrentRevision, "design", proposals[:1], time.Now()) },
		"more than the stage allows": func(r *Run) error {
			return r.Branch(r.CurrentRevision, "design", append(proposals, Variation{Name: "a"}, Variation{Name: "b"}), time.Now())
		},
		"an older revision": func(r *Run) error { return r.Branch("rev_old", "design", proposals, time.Now()) },
		"a stage not yet accepted": func(r *Run) error {
			delete(r.Current().Checkpoints, "design")
			return r.Branch(r.CurrentRevision, "design", proposals, time.Now())
		},
		"a variation branching again": func(r *Run) error {
			if err := r.Branch(r.CurrentRevision, "design", proposals, time.Now()); err != nil {
				return nil
			}
			return r.Branch(r.CurrentRevision, "design", proposals, time.Now())
		},
	}
	for name, branch := range cases {
		if err := branch(variationRun(t)); err == nil {
			t.Fatalf("branched on %s", name)
		}
	}
}

// finishExceptChange puts a variation where a person would compare it: every
// stage but its approved change accepted, with readiness satisfied. Without
// readiness nothing is ready for any reason, which would let a test about
// holding back approval pass without testing anything.
func finishExceptChange(rev *Revision) {
	now := time.Now()
	rev.State, rev.Runtime = "active", RuntimeState{ID: "vm-" + rev.ID, Ready: true}
	for id, n := range rev.Config.Workflow.Nodes {
		if n.Kind != "change" {
			rev.Checkpoints[id] = Checkpoint{ID: "cp_" + id + rev.ID, Node: id}
		}
	}
	for _, c := range rev.Requirements() {
		rev.SetProbe(Probe{Capability: c, Binding: "builtin@1", Passed: true, ConfigDigest: Digest(rev.Config), RuntimeID: rev.Runtime.ID,
			EvidenceDigest: Digest(c), CheckedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)})
	}
}

func TestNoVariationCanReachApprovalBeforeAPersonChooses(t *testing.T) {
	r := variationRun(t)
	if err := r.Branch(r.CurrentRevision, "design", proposals, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := range r.Revisions {
		finishExceptChange(&r.Revisions[i])
		for _, id := range r.Revisions[i].ReadyNodes(time.Now()) {
			if r.Revisions[i].Config.Workflow.Nodes[id].Kind == "change" {
				t.Fatalf("an undecided variation readied its approved change %s", id)
			}
		}
	}
}

func TestChoosingContinuesOneVariationAndRetiresTheRest(t *testing.T) {
	r := variationRun(t)
	if err := r.Branch(r.CurrentRevision, "design", proposals, time.Now()); err != nil {
		t.Fatal(err)
	}
	chosen, other := r.Revisions[1].ID, r.Revisions[2].ID
	// A variation still working cannot be chosen: there is nothing to compare.
	if err := r.Choose(chosen, time.Now()); err == nil || !strings.Contains(err.Error(), "has not finished") {
		t.Fatalf("chose an unfinished variation: %v", err)
	}
	for i := range r.Revisions {
		finishExceptChange(&r.Revisions[i])
	}
	r.Revision(other).Attempts = []Attempt{{ID: "a_run", Node: "qa", State: "running"}}

	if err := r.Choose(chosen, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.CurrentRevision != chosen || !r.Revision(chosen).Variant.Chosen {
		t.Fatal("the chosen variation did not become current")
	}
	// Its approved change is now allowed to run.
	ready := r.Revision(chosen).ReadyNodes(time.Now())
	if len(ready) != 1 || ready[0] != "approved-change" {
		t.Fatalf("the chosen variation cannot proceed to approval: %v", ready)
	}
	for _, id := range []string{r.Revisions[0].ID, other} {
		rev := r.Revision(id)
		if rev.State != NotChosen || rev.Undecided() || len(rev.Checkpoints) == 0 {
			t.Fatalf("a variation not chosen is not retired with its evidence kept: %s %+v", rev.State, rev.Variant)
		}
	}
	if a := r.Revision(other).Attempt("a_run"); a.State != "cancelled" {
		t.Fatalf("a retired variation's running work was not stopped: %s", a.State)
	}
	if err := r.Choose(other, time.Now()); err == nil {
		t.Fatal("a comparison was decided twice")
	}
}

func TestSteeringAVariationReachesThatVariation(t *testing.T) {
	// Messages used to be written to the current revision whatever was
	// addressed, which during a comparison is a different variation.
	r := variationRun(t)
	if err := r.Branch(r.CurrentRevision, "design", proposals, time.Now()); err != nil {
		t.Fatal(err)
	}
	sibling := r.Revisions[2].ID
	before := len(r.Current().Messages)
	if err := r.Message(sibling, "design", "worker", "prefer the standard library", time.Now()); err != nil {
		t.Fatal(err)
	}
	s := r.Revision(sibling).Messages
	if s[len(s)-1].Body != "prefer the standard library" {
		t.Fatal("steering did not reach the addressed variation")
	}
	if len(r.Current().Messages) != before {
		t.Fatal("steering one variation changed another")
	}
}
