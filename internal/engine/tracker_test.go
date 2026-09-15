package engine

import (
	"context"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// setupTracked mirrors setupWorkflow but configures a Linear tracker and
// links the run to a task ref, so AppendTrackerLog's call sites actually
// enqueue entries.
func setupTracked(t *testing.T) *harness {
	t.Helper()
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c, err := workflow.Parse([]byte("version: 2\nproject: test\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\ntracker: {provider: linear, credential: \"env:ENVCTL_ENGINE_TRACKER_TEST\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	run, err := workflow.NewRun("test", "exercise workflow", "developer", c, now)
	if err != nil {
		t.Fatal(err)
	}
	run.TaskRef = "ENG-99"
	run.Current().State = "preparing"
	run.Current().Runtime = workflow.RuntimeState{ID: "envctl-test", Provider: "lima", Location: "local", State: "preparing"}
	if _, err = store.Create(context.Background(), "create", "fixture", run); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, id: run.ID, rev: run.CurrentRevision, now: now}
	b := &fixtureBackend{store: store, starts: map[string]int{}, observations: map[string]Observation{}, now: func() time.Time { return h.now }}
	h.backend = b
	h.engine = &Engine{Store: store, Backend: b, Now: b.now}
	return h
}

func entriesOfKind(run *workflow.Run, kind string) []workflow.TrackerLogEntry {
	var out []workflow.TrackerLogEntry
	for _, e := range run.TrackerLog {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

func TestTrackerLogRecordsStageCompletionApprovalAndPublication(t *testing.T) {
	h := setupTracked(t)
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.State == "awaiting-approval" {
				return true
			}
		}
		return false
	})
	run := h.run()
	if len(entriesOfKind(run, workflow.TrackerKindStageCompleted)) == 0 {
		t.Fatal("no stage_completed entries recorded before approval")
	}
	if len(entriesOfKind(run, workflow.TrackerKindAwaitingApproval)) != 1 {
		t.Fatalf("expected exactly one awaiting_approval entry, got %d", len(entriesOfKind(run, workflow.TrackerKindAwaitingApproval)))
	}

	v := run.Current()
	a := v.Attempts[len(v.Attempts)-1]
	// Approval itself is logged by the daemon action handler (internal/daemon),
	// not the engine; exercised there. Here, approving lets the change stage
	// publish, which the engine does log as a further stage_completed entry.
	h.mutate(func(r *workflow.Run) error { return r.Approve(h.rev, a.ID, "developer", a.Result.WorkDigest(), h.now) })
	h.until(func(v *workflow.Revision) bool { return v.State == "completed" })
	for i := 0; i < 3; i++ {
		h.step()
	}
	run = h.run()
	published := entriesOfKind(run, workflow.TrackerKindStageCompleted)
	found := false
	for _, e := range published {
		cp := run.Current().Checkpoints[e.Node]
		if len(cp.Result.PRs) > 0 {
			found = true
		}
	}
	if !found {
		t.Fatal("no stage_completed entry recorded for the published change with a PR")
	}
}

func TestTrackerLogRecordsNeedsAttentionOnAttemptBudgetExhaustion(t *testing.T) {
	h := setupTracked(t)
	h.mutate(func(r *workflow.Run) error { r.Current().Config.Limits.MaxAttempts = 1; return nil })
	h.until(func(v *workflow.Revision) bool { return len(v.Attempts) > 0 })
	a := h.run().Current().Attempts[0]
	h.backend.observations[a.ID] = Observation{State: "failed", Detail: "fixture failure"}
	h.until(func(v *workflow.Revision) bool { return v.State == "needs-attention" })
	run := h.run()
	if len(entriesOfKind(run, workflow.TrackerKindNeedsAttention)) != 1 {
		t.Fatalf("expected exactly one needs_attention entry, got %d", len(entriesOfKind(run, workflow.TrackerKindNeedsAttention)))
	}
}

func TestTrackerLogEmptyWithoutTaskRef(t *testing.T) {
	h := setupTracked(t)
	run := h.run()
	if _, err := h.engine.Store.Mutate(context.Background(), h.id, run.Version, workflow.ID("test"), "test.clearref", nil, func(r *workflow.Run) error {
		r.TaskRef = ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.State == "awaiting-approval" {
				return true
			}
		}
		return false
	})
	v := h.run().Current()
	a := v.Attempts[len(v.Attempts)-1]
	h.mutate(func(r *workflow.Run) error { return r.Approve(h.rev, a.ID, "developer", a.Result.WorkDigest(), h.now) })
	h.until(func(v *workflow.Revision) bool { return v.State == "completed" })
	for i := 0; i < 3; i++ {
		h.step()
	}
	if len(h.run().TrackerLog) != 0 {
		t.Fatal("tracker log entries recorded for a run without a task ref")
	}
}
