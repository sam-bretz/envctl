package localexec

import (
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestJoinSelectsDeclaredStartAndDatasetWithoutLosingLineage(t *testing.T) {
	_, a := fixture(t)
	a.Attempt.Inputs = map[string]string{"left": "cp_left", "right": "cp_right"}
	n := a.Revision.Config.Workflow.Nodes[a.Attempt.Node]
	n.Needs = []string{"left", "right"}
	n.Join = &workflow.JoinPolicy{Repositories: map[string]string{"app": "left"}, Datasets: map[string]string{"db": "right"}}
	a.Revision.Config.Workflow.Nodes[a.Attempt.Node] = n
	a.Revision.Checkpoints = map[string]workflow.Checkpoint{}
	for _, parent := range n.Needs {
		a.Revision.Checkpoints[parent] = workflow.Checkpoint{ID: "cp_" + parent, Result: workflow.Result{Commits: map[string]string{"app": workflow.Digest(parent)[:40]}, Datasets: map[string]workflow.DatasetSnapshot{"db": {Source: workflow.Artifact{Digest: workflow.Digest(parent)}}}}}
	}
	for i := 0; i < 50; i++ {
		pins, err := inputCommits(a)
		if err != nil || pins["app"] != a.Revision.Checkpoints["left"].Result.Commits["app"] {
			t.Fatal("join selected an arbitrary source", err)
		}
		data, err := inputDatasets(a)
		if err != nil || data["db"].Source.Digest != workflow.Digest("right") {
			t.Fatal("join selected an arbitrary dataset", err)
		}
	}
	bad := workflow.Clone(a)
	delete(bad.Attempt.Inputs, "right")
	if _, err := inputCommits(bad); err == nil {
		t.Fatal("missing merge parent accepted")
	}
	if _, err := inputDatasets(bad); err == nil {
		t.Fatal("missing dataset parent accepted")
	}
	bad = workflow.Clone(a)
	cp := bad.Revision.Checkpoints["right"]
	cp.HistoricalOnly = true
	bad.Revision.Checkpoints["right"] = cp
	if _, err := inputCommits(bad); err == nil {
		t.Fatal("historical merge parent accepted")
	}
	if _, err := inputDatasets(bad); err == nil {
		t.Fatal("historical dataset parent accepted")
	}
	bad = workflow.Clone(a)
	delete(bad.Revision.Checkpoints["right"].Result.Datasets, "db")
	if _, err := inputDatasets(bad); err == nil {
		t.Fatal("missing declared snapshot accepted")
	}
	bad = workflow.Clone(a)
	bad.Revision.Config.Workflow.Nodes[a.Attempt.Node].Join.Datasets = nil
	if _, err := inputDatasets(bad); err == nil {
		t.Fatal("divergent snapshots implicitly joined")
	}
}
