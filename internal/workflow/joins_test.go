package workflow

import (
	"strings"
	"testing"
	"time"
)

func joinConfig(t *testing.T) Config {
	c := fixture(t)
	c.Workflow.Nodes = map[string]Node{
		"task":  {Kind: "task", Outputs: []string{"task"}},
		"plan":  {Kind: "plan", Needs: []string{"task"}, Outputs: []string{"plan"}},
		"left":  {Kind: "code", Needs: []string{"plan"}, Outputs: []string{"left"}, Writes: []string{"app"}},
		"right": {Kind: "code", Needs: []string{"plan"}, Outputs: []string{"right"}, Writes: []string{"app"}},
		"merge": {Kind: "code", Needs: []string{"left", "right"}, Outputs: []string{"merged"}, Writes: []string{"app"}, Checks: []Check{{Name: "unit", Command: []string{"make", "test"}}}, Join: &JoinPolicy{Repositories: map[string]string{"app": "left"}}},
	}
	return c
}

func TestJoinOwnershipMustBeResolvedInConfiguration(t *testing.T) {
	c := joinConfig(t)
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Node){
		"implicit merge":     func(n *Node) { n.Join = nil },
		"missing permission": func(n *Node) { n.Writes = nil },
		"unknown repository": func(n *Node) { n.Join.Repositories["unknown"] = "left" },
		"non-input base":     func(n *Node) { n.Join.Repositories["app"] = "plan" },
		"no checks":          func(n *Node) { n.Checks = nil },
		"QA writing":         func(n *Node) { n.Kind = "qa" },
		"unknown dataset":    func(n *Node) { n.Join.Datasets = map[string]string{"unknown": "left"} },
	} {
		t.Run(name, func(t *testing.T) {
			bad := Clone(c)
			n := bad.Workflow.Nodes["merge"]
			mutate(&n)
			bad.Workflow.Nodes["merge"] = n
			if err := bad.Validate(); err == nil {
				t.Fatal("ambiguous join accepted")
			}
		})
	}
	// Read-only fan-out has a single source identity and needs no source merge.
	for _, id := range []string{"left", "right", "merge"} {
		n := c.Workflow.Nodes[id]
		n.Writes, n.Join = nil, nil
		c.Workflow.Nodes[id] = n
	}
	if err := c.Validate(); err != nil {
		t.Fatal("read-only diamond rejected", err)
	}
	// A writer on only one arm still requires a declared choice/merge at the
	// join. The read-only arm cannot silently overwrite the changed source.
	n := c.Workflow.Nodes["left"]
	n.Writes = []string{"app"}
	c.Workflow.Nodes["left"] = n
	if err := c.Validate(); err == nil {
		t.Fatal("mixed writer/read-only join accepted without policy")
	}
}

func TestMergeResultBindsEveryInputAndSupervisor(t *testing.T) {
	r, err := NewRun("join", "combine changes", "dev", joinConfig(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v := r.Current()
	v.State = "active"
	v.Runtime.Ready = true
	ready(v, time.Now())
	for _, id := range []string{"left", "right"} {
		v.Checkpoints[id] = Checkpoint{ID: "cp_" + id, Result: Result{Commits: map[string]string{"app": Digest(id)[:40]}}}
	}
	attempt, err := v.Begin("merge", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	result := output(v, "merge")
	result.Sources = map[string]Artifact{"app": {Name: "source", Digest: Digest("bundle"), Size: 20, MediaType: "application/x-git-bundle"}}
	result.Checks = []CheckResult{{Name: "unit", Passed: true, EvidenceDigest: Digest("checks"), CommitsDigest: Digest(result.Commits)}}
	result.MergeParents, err = v.MergeInputs("merge")
	if err != nil {
		t.Fatal(err)
	}
	review(&result)
	for name, mutate := range map[string]func(*Result){
		"missing":        func(r *Result) { r.MergeParents = nil },
		"dropped branch": func(r *Result) { delete(r.MergeParents["app"], "right") },
		"wrong pin":      func(r *Result) { r.MergeParents["app"]["right"] = strings.Repeat("f", 40) },
		"missing bundle": func(r *Result) { r.Sources = nil },
	} {
		t.Run(name, func(t *testing.T) {
			bad := Clone(result)
			mutate(&bad)
			review(&bad)
			if err := v.Propose(attempt.ID, bad, time.Now()); err == nil {
				t.Fatal("unbound merge result accepted")
			}
		})
	}
	if err := v.Propose(attempt.ID, result, time.Now()); err != nil {
		t.Fatal(err)
	}
	cp, err := v.Accept(attempt.ID, time.Now())
	if err != nil || len(cp.Inputs) != 2 {
		t.Fatal("join did not preserve checkpoint parents", err)
	}
}
