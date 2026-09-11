package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Reviews a completed real feature from retained state only. This deliberately
// never connects to its VM, harness, source repositories, or publication API.
func TestRetainedFeatureCheckpointComparison(t *testing.T) {
	dir, id := os.Getenv("ENVCTL_REVIEW_STORE"), os.Getenv("ENVCTL_REVIEW_RUN")
	if dir == "" || id == "" {
		t.Skip("explicit retained feature store and run required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	s, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r, err := s.Get(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	before := workflow.Digest(r)
	plan, ok := r.Current().Checkpoints["plan"]
	if !ok {
		t.Fatal("Plan checkpoint missing")
	}
	code, ok := r.Current().Checkpoints["code"]
	if !ok {
		t.Fatal("Code checkpoint missing")
	}
	c, err := Compare(ctx, s, id, Request{Node: "code", From: plan.ID})
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, repo := range c.Repositories {
		if repo.Unavailable != "" || repo.Truncated {
			t.Fatal("real source comparison incomplete", repo.Repository, repo.Unavailable)
		}
		if repo.Before != repo.After {
			if repo.Patch == "" {
				t.Fatal("changed commit has no source diff")
			}
			changed++
		}
	}
	if len(c.Repositories) < 2 || changed == 0 {
		t.Fatal("fixture did not prove multi-repository source review")
	}
	qa, err := Compare(ctx, s, id, Request{Node: "qa", From: code.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(qa.To.Checkpoint.Result.Checks) < 3 || len(qa.To.Checkpoint.Result.Datasets) < 2 {
		t.Fatal("QA evidence or datasets absent")
	}
	for _, repo := range qa.Repositories {
		if repo.Before != repo.After || repo.Unavailable != "" {
			t.Fatal("QA source differs from Code")
		}
	}
	historical := ""
	for _, rev := range r.Revisions {
		if cp, ok := rev.Checkpoints["plan"]; ok && cp.HistoricalOnly {
			historical = cp.ID
			break
		}
	}
	if historical == "" {
		t.Fatal("fixture has no historical Plan")
	}
	history, err := Compare(ctx, s, id, Request{Node: "plan", From: historical})
	if err != nil {
		t.Fatal(err)
	}
	if !history.From.Checkpoint.HistoricalOnly || history.From.ConfigDigest == history.To.ConfigDigest {
		t.Fatal("plugin amendment history not reflected")
	}
	after, err := s.Get(ctx, id)
	if err != nil || workflow.Digest(after) != before {
		t.Fatal("review changed retained workflow", err)
	}
	raw, err := json.MarshalIndent(map[string]any{"run": id, "workflow_digest": before, "code_vs_plan": c, "qa_vs_code": qa, "plan_amendment": history}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "acceptance-review.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("retained feature review: %d repositories, %d changed; QA checks/datasets and historical Plan amendment verified without VM or source checkout", len(c.Repositories), changed)
}
