package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestComparisonAPIIsScopedAndReadOnly(t *testing.T) {
	client, store := testAPI(t)
	ctx := context.Background()
	r := create(t, client)
	r, err := store.Mutate(ctx, r.ID, r.Version, "review-fixture", "fixture", nil, func(r *workflow.Run) error {
		r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_old", Revision: r.CurrentRevision, Node: "task", Result: workflow.Result{Summary: "original"}}
		if _, err := r.Rewind("task", "revised", nil, time.Now()); err != nil {
			return err
		}
		r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_new", Revision: r.CurrentRevision, Node: "task", Result: workflow.Result{Summary: "revised"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c, err := client.Diff(ctx, r.ID, review.Request{Revision: r.CurrentRevision, Node: "task", From: "cp_old"})
	if err != nil || c.From.Checkpoint.ID != "cp_old" || c.To.Checkpoint.ID != "cp_new" || c.From.Objective == c.To.Objective {
		t.Fatal("API lost comparison selectors", err)
	}
	if _, err := client.Diff(ctx, r.ID, review.Request{Revision: "missing", Node: "task"}); err == nil {
		t.Fatal("unknown revision accepted")
	}
	if _, err := client.Diff(ctx, r.ID, review.Request{Node: "task", From: "foreign"}); err == nil {
		t.Fatal("unknown checkpoint accepted")
	}
	after, err := client.Get(ctx, r.ID)
	if err != nil || after.Version != r.Version {
		t.Fatal("comparison mutated workflow", err)
	}
}
