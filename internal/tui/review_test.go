package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRevisionReviewIsIndependentAndReadOnly(t *testing.T) {
	m := modelFixture(t)
	r := &m.Runs[0]
	old := r.CurrentRevision
	r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_old", Node: "task", Result: workflow.Result{Summary: "original task", Artifacts: []workflow.Artifact{{Name: "task", Digest: "old", Size: 8}}}}
	if _, err := r.Rewind("task", "revised task", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_new", Node: "task", Result: workflow.Result{Summary: "revised task"}}
	m.Panel = 1
	m, cmd := key(m, "[")
	if cmd != nil || m.viewRevision().ID != old || !strings.Contains(m.details(), "original task") {
		t.Fatal("history did not select the old checkpoint")
	}
	m, cmd = key(m, "x")
	if cmd == nil {
		t.Fatal("historical mutation has no feedback")
	}
	message, ok := cmd().(actionMsg)
	if !ok || message.err == nil || m.API.(*fakeAPI).request.Action != "" {
		t.Fatal("history sent a mutation to current execution")
	}
	copy := m
	copy, _ = key(copy, "]")
	if copy.viewRevision().ID != r.CurrentRevision || m.viewRevision().ID != old {
		t.Fatal("client view selection leaked between clients")
	}
	if !strings.Contains(copy.details(), "revised task") {
		t.Fatal("returning to current revision lost its result")
	}
}

func TestArtifactResponseCannotCrossReviewSelection(t *testing.T) {
	m := modelFixture(t)
	m.Runs[0].Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_task", Node: "task", Result: workflow.Result{Artifacts: []workflow.Artifact{{Name: "first", Digest: "a"}, {Name: "second", Digest: "b"}}}}
	m, load := key(m, "o")
	if load == nil {
		t.Fatal("artifact not requested")
	}
	m, _ = key(m, ".")
	next, _ := m.Update(load())
	m = next.(Model)
	if m.ArtifactText != "" {
		t.Fatal("first artifact response appeared under second selection")
	}
	m, load = key(m, "o")
	next, _ = m.Update(load())
	m = next.(Model)
	if m.ArtifactText != "artifact" {
		t.Fatal("selected artifact was not shown")
	}
	m, load = key(m, "o")
	m, _ = key(m, "tab")
	next, _ = m.Update(load())
	if next.(Model).ArtifactText != "" {
		t.Fatal("late response replaced another panel")
	}
}

func TestSteeringCannotMoveToReplacementRevisionDuringInput(t *testing.T) {
	m := modelFixture(t)
	m, _ = key(m, "i")
	m.Input = "Follow the original decision"
	r := workflow.Clone(m.Runs[0])
	if _, err := r.Rewind("plan", "new decision", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	next, _ := m.Update(snapshotMsg{runs: []workflow.Run{r}})
	m, cmd := key(next.(Model), "enter")
	if cmd != nil || m.Error == "" || m.API.(*fakeAPI).request.Action != "" {
		t.Fatal("message intended for old revision reached the replacement")
	}
}

func TestScreenshotEvidenceHasReadableTerminalPreview(t *testing.T) {
	preview := artifactPreview([]byte(`{"ok":true,"output":{"screenshot_png":"large-encoded-image","title":"Addition"}}`))
	if strings.Contains(preview, "large-encoded-image") || !strings.Contains(preview, "Addition") || !strings.Contains(preview, "PNG screenshot retained") {
		t.Fatal("screenshot hid useful check evidence")
	}
}

func TestSmallTerminalPreservesReviewContentAndDetach(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 30, 8
	view := m.View().Content
	if !strings.Contains(view, "No stage messages") || !strings.Contains(view, "q detach") {
		t.Fatal("compact layout hid content or detach control", view)
	}
}

func TestCheckpointComparisonUsesIndependentHistoricalBase(t *testing.T) {
	m := modelFixture(t)
	r := &m.Runs[0]
	old := r.CurrentRevision
	r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_old", Node: "task"}
	if _, err := r.Rewind("task", "new objective", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_new", Node: "task", Result: workflow.Result{Artifacts: []workflow.Artifact{{Name: "task", Digest: "a"}}}}
	m, _ = key(m, "[")
	m, _ = key(m, "b")
	if m.CompareFrom != "cp_old" || m.viewRevision().ID != old {
		t.Fatal("historical comparison base not selected")
	}
	m, _ = key(m, "]")
	m, load := key(m, "d")
	if load == nil {
		t.Fatal("diff not requested")
	}
	next, _ := m.Update(load())
	m = next.(Model)
	api := m.API.(*fakeAPI)
	if api.diff.From != "cp_old" || api.diff.Revision != r.CurrentRevision || !strings.Contains(m.details(), "+new") || api.request.Action != "" {
		t.Fatal("comparison did not use scoped read-only request")
	}
	m, _ = key(m, "B")
	m, load = key(m, "d")
	load()
	if api.diff.From != "" {
		t.Fatal("base reset did not restore source pin comparison")
	}
}

func TestLateDiffCannotReplaceArtifactOrChangedSelection(t *testing.T) {
	m := modelFixture(t)
	m.Runs[0].Current().Checkpoints["task"] = workflow.Checkpoint{ID: "cp_task", Node: "task", Result: workflow.Result{Artifacts: []workflow.Artifact{{Name: "task", Digest: "a"}}}}
	m, load := key(m, "d")
	m, artifact := key(m, "o")
	next, _ := m.Update(artifact())
	m = next.(Model)
	next, _ = m.Update(load())
	m = next.(Model)
	if m.ArtifactText != "artifact" {
		t.Fatal("late diff replaced selected artifact")
	}
	m, load = key(m, "d")
	m, _ = key(m, "tab")
	next, _ = m.Update(load())
	m = next.(Model)
	if m.ArtifactText != "" {
		t.Fatal("late diff crossed panel selection")
	}
	m, load = key(m, "d")
	m, _ = key(m, "esc")
	next, _ = m.Update(load())
	m = next.(Model)
	if m.ArtifactText != "" {
		t.Fatal("closed comparison reappeared")
	}
}
