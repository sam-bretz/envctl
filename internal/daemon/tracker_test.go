package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func createTracked(t *testing.T, c *Client, taskRef string) *workflow.Run {
	t.Helper()
	config, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\ntracker: {provider: linear, credential: \"env:ENVCTL_DAEMON_TRACKER_TEST\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Create(context.Background(), CreateRequest{OperationID: "create", Task: "Build exports", Name: "Exports", TaskRef: taskRef, Owner: "test", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func result(t *testing.T) workflow.Result {
	t.Helper()
	r := workflow.Result{Summary: "Done", Commits: map[string]string{"app": "0000000000000000000000000000000000000a"}}
	r.Artifacts = append(r.Artifacts, workflow.Artifact{Name: "task", Digest: workflow.Digest("task"), Size: 5})
	r.Review = workflow.Review{Accepted: true, Summary: "Reviewed", EvidenceDigest: workflow.Digest("review"), ResultDigest: r.WorkDigest()}
	return r
}

func TestApproveRecordsTrackerLogEntry(t *testing.T) {
	client, store := testAPI(t)
	ctx := context.Background()
	r := createTracked(t, client, "ENG-1")
	res := result(t)
	r, err := store.Mutate(ctx, r.ID, r.Version, "seed", "fixture", nil, func(r *workflow.Run) error {
		rev := r.Current()
		rev.Attempts = append(rev.Attempts, workflow.Attempt{ID: "att_1", Node: "task", Number: 1, State: "awaiting-approval", Result: &res})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.Action(ctx, r.ID, ActionRequest{OperationID: "approve", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "approve", Attempt: "att_1", Actor: "dev", WorkDigest: res.WorkDigest()})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.TrackerLog) != 1 || updated.TrackerLog[0].Kind != workflow.TrackerKindApproved || updated.TrackerLog[0].Attempt != "att_1" {
		t.Fatalf("approved entry not recorded: %+v", updated.TrackerLog)
	}
}

func TestApproveWithoutTaskRefRecordsNoTrackerLogEntry(t *testing.T) {
	client, store := testAPI(t)
	ctx := context.Background()
	r := createTracked(t, client, "") // no task ref: tracker configured but unlinked
	res := result(t)
	r, err := store.Mutate(ctx, r.ID, r.Version, "seed", "fixture", nil, func(r *workflow.Run) error {
		rev := r.Current()
		rev.Attempts = append(rev.Attempts, workflow.Attempt{ID: "att_1", Node: "task", Number: 1, State: "awaiting-approval", Result: &res})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.Action(ctx, r.ID, ActionRequest{OperationID: "approve", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "approve", Attempt: "att_1", Actor: "dev", WorkDigest: res.WorkDigest()})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.TrackerLog) != 0 {
		t.Fatalf("run without a task ref posted nothing, but got: %+v", updated.TrackerLog)
	}
}

func TestRewindRecordsTrackerLogEntryWithTargetAndObjectiveChange(t *testing.T) {
	client, _ := testAPI(t)
	ctx := context.Background()
	r := createTracked(t, client, "ENG-2")

	updated, err := client.Action(ctx, r.ID, ActionRequest{OperationID: "rewind", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "rewind", Node: "task", Task: "A revised objective"})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.TrackerLog) != 1 {
		t.Fatalf("expected one rewound entry, got %+v", updated.TrackerLog)
	}
	e := updated.TrackerLog[0]
	if e.Kind != workflow.TrackerKindRewound || e.Node != "task" || e.Revision != updated.CurrentRevision {
		t.Fatalf("unexpected rewound entry: %+v", e)
	}
	if !strings.Contains(e.Detail, "target task") || !strings.Contains(e.Detail, "objective changed") {
		t.Fatalf("rewound entry detail missing target/objective: %q", e.Detail)
	}
}

func TestRewindWithoutObjectiveChangeOmitsObjectiveNote(t *testing.T) {
	client, _ := testAPI(t)
	ctx := context.Background()
	r := createTracked(t, client, "ENG-3")

	updated, err := client.Action(ctx, r.ID, ActionRequest{OperationID: "rewind", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "rewind", Node: "plan"})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.TrackerLog) != 1 {
		t.Fatalf("expected one rewound entry, got %+v", updated.TrackerLog)
	}
	if e := updated.TrackerLog[0]; strings.Contains(e.Detail, "objective changed") {
		t.Fatalf("unexpected objective-changed note: %q", e.Detail)
	}
}

func TestNeedsAttentionOnRecoveryRecordsTrackerLogEntry(t *testing.T) {
	client, store := testAPI(t)
	ctx := context.Background()
	r := createTracked(t, client, "ENG-4")
	_, err := store.Mutate(ctx, r.ID, r.Version, "seed", "fixture", nil, func(r *workflow.Run) error {
		rev := r.Current()
		rev.State = "needs-attention"
		r.AppendTrackerLog(rev.Config.Tracker, workflow.TrackerKindNeedsAttention, rev.ID, "", "", "stage code exhausted its configured attempts", time.Now())
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := client.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.TrackerLog) != 1 || updated.TrackerLog[0].Kind != workflow.TrackerKindNeedsAttention {
		t.Fatalf("needs_attention entry not recorded: %+v", updated.TrackerLog)
	}
}
