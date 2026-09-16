package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type fakeDiffer struct{ unavailable map[string]bool }

func (f fakeDiffer) Diff(_ context.Context, _ string, req review.Request) (review.Comparison, error) {
	if f.unavailable[req.Revision] {
		return review.Comparison{}, errors.New("source bundle missing")
	}
	return review.Comparison{Repositories: []review.RepositoryDiff{{Patch: "diff --git a/x b/x\n--- a/x\n+++ b/x\n+one\n+two\n-three\n"}}}, nil
}

func comparedRun(t *testing.T) *workflow.Run {
	t.Helper()
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature, nodes: {design: {variations: 3}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := workflow.NewRun("demo", "ship it", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range []string{"task", "plan", "design"} {
		r.Current().Checkpoints[node] = workflow.Checkpoint{ID: "cp_" + node, Node: node, Result: workflow.Result{Review: workflow.Review{Summary: "sound"}}}
	}
	if err = r.Branch(r.CurrentRevision, "design", []workflow.Variation{{Name: "polling", Rationale: "no webhooks upstream"}, {Name: "streaming", Rationale: "at high volume"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	for i := range r.Revisions {
		v := &r.Revisions[i]
		for _, node := range []string{"design", "code", "qa"} {
			v.Checkpoints[node] = workflow.Checkpoint{ID: "cp_" + node + v.ID, Node: node, Result: workflow.Result{
				Summary: v.Variant.Name + " " + node, Commits: map[string]string{"app": strings.Repeat("a", 40)},
			}}
		}
		v.Checkpoints["qa"] = workflow.Checkpoint{ID: "cp_qa" + v.ID, Node: "qa", Result: workflow.Result{
			Summary: v.Variant.Name + " qa", Commits: map[string]string{"app": strings.Repeat("b", 40)},
			Checks: []workflow.CheckResult{{Name: "unit", Passed: v.Variant.Name != "streaming", ExitCode: 1}},
		}}
		v.Attempts = []workflow.Attempt{{ID: "a_" + v.ID, Node: "qa", State: "checkpointed", Usage: &workflow.Usage{Input: int64(1000 * (i + 1))}}}
	}
	return r
}

func TestRunVariationsShowsEachCandidateSideBySide(t *testing.T) {
	r := comparedRun(t)
	streaming, err := r.FindVariation("streaming")
	if err != nil {
		t.Fatal(err)
	}
	compared, err := review.CompareVariations(context.Background(), fakeDiffer{unavailable: map[string]bool{streaming.ID: true}}, r)
	if err != nil {
		t.Fatal(err)
	}
	if len(compared) != 3 {
		t.Fatalf("want 3 candidates, got %d", len(compared))
	}
	text := review.VariationsText(compared)
	for _, want := range []string{
		"as proposed  [ready to choose]",
		"polling  [ready to choose]", "why: no webhooks upstream",
		"code: polling code",
		// The change is sized from retained evidence against the last stage
		// with commits, and a missing bundle is reported rather than hidden.
		"change: 1 files, +2 -1", "change: unavailable (source bundle missing)",
		"commit app: bbbbbbbbbbbb",
		"check qa/unit: passed", "check qa/unit: failed (exit 1)",
		"tokens: ",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in:\n%s", want, text)
		}
	}
}

func TestAVariationIsFoundByNameOrRevision(t *testing.T) {
	r := comparedRun(t)
	byName, err := r.FindVariation("Polling")
	if err != nil || byName.Variant.Name != "polling" {
		t.Fatalf("name lookup: %v %+v", err, byName)
	}
	if byID, err := r.FindVariation(byName.ID); err != nil || byID.ID != byName.ID {
		t.Fatalf("revision lookup: %v", err)
	}
	if _, err := r.FindVariation("nope"); err == nil {
		t.Fatal("found a variation that does not exist")
	}
}

func TestRunVariationsExplainsARunWithoutAny(t *testing.T) {
	c, _ := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	r, _ := workflow.NewRun("demo", "x", "dev", c, time.Now())
	if _, err := review.CompareVariations(context.Background(), fakeDiffer{}, r); err == nil || !strings.Contains(err.Error(), "variations in envctl.yaml") {
		t.Fatalf("a run without variations was not explained: %v", err)
	}
}

func TestRunShowSaysAComparisonIsWaitingAndHowToAct(t *testing.T) {
	r := comparedRun(t)
	var out strings.Builder
	printVariationStatus(&out, r)
	for _, want := range []string{"variations:", "polling", "ready to choose", "envctl run variations " + r.ID, "envctl run choose " + r.ID + " --variation <name>"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
	polling, _ := r.FindVariation("polling")
	if err := r.Choose(polling.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	printVariationStatus(&out, r)
	if strings.Contains(out.String(), "envctl run choose") {
		t.Fatalf("still says to choose after a choice was made:\n%s", out.String())
	}
}
