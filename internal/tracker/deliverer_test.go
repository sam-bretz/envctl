package tracker

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type fakeTracker struct {
	mu    sync.Mutex
	fail  int // remaining calls that return an error before succeeding
	calls int
	id    string
}

func (f *fakeTracker) Comment(ctx context.Context, ref, marker, body string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail > 0 {
		f.fail--
		return "", errors.New("temporary linear outage")
	}
	id := f.id
	if id == "" {
		id = "comment_" + marker
	}
	return id, nil
}
func (f *fakeTracker) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func delivererFixture(t *testing.T) (*runstore.Store, *workflow.Run) {
	t.Helper()
	t.Setenv("LINEAR_TOKEN_DELIVERER_TEST", "secret-key")
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cfg, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\ntracker: {provider: linear, credential: \"env:LINEAR_TOKEN_DELIVERER_TEST\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("demo", "objective", "dev", cfg, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.TaskRef = "ENG-1"
	return store, run
}

func TestDelivererDeliversAndRetriesOnFailure(t *testing.T) {
	store, run := delivererFixture(t)
	run.TrackerLog = []workflow.TrackerLogEntry{{ID: "tlog_1", Kind: workflow.TrackerKindNeedsAttention, Revision: run.CurrentRevision, Detail: "token ceiling reached", Status: "pending"}}
	ctx := context.Background()
	run, err := store.Create(ctx, "op-create", nil, run)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeTracker{fail: 2}
	clock := time.Now()
	d := &Deliverer{Store: store, Now: func() time.Time { return clock }, newTracker: func(workflow.TrackerConfig) (Tracker, error) { return fake, nil }}

	for i := 0; i < 5; i++ {
		d.tick(ctx)
		clock = clock.Add(time.Minute)
	}
	got, err := store.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TrackerLog) != 1 || got.TrackerLog[0].Status != "posted" || got.TrackerLog[0].CommentID == "" {
		t.Fatalf("entry not delivered: %+v", got.TrackerLog)
	}
	if fake.count() != 3 {
		t.Fatalf("expected 2 failures + 1 success, got %d calls", fake.count())
	}

	// A later tick after delivery must not call the tracker again.
	d.tick(ctx)
	if fake.count() != 3 {
		t.Fatal("delivered entry was redelivered")
	}
}

func TestDelivererReceiptReconciliationAvoidsDuplicateRemoteCall(t *testing.T) {
	store, run := delivererFixture(t)
	run.TrackerLog = []workflow.TrackerLogEntry{{ID: "tlog_1", Kind: workflow.TrackerKindNeedsAttention, Revision: run.CurrentRevision, Detail: "token ceiling reached", Status: "pending"}}
	ctx := context.Background()
	run, err := store.Create(ctx, "op-create", nil, run)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeTracker{}
	d := &Deliverer{Store: store, newTracker: func(workflow.TrackerConfig) (Tracker, error) { return fake, nil }}

	// Pre-render the exact body a real delivery would produce, and freeze a
	// receipt as if a prior process crashed after a successful remote create
	// but before recording the receipt's local success.
	body, err := Render(run, run.TrackerLog[0])
	if err != nil {
		t.Fatal(err)
	}
	digest := workflow.Digest(body)
	path := d.receiptPath(run.ID, "tlog_1")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(trackerReceipt{CommentID: "already-posted-id", BodyDigest: digest})
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}

	d.tick(ctx)
	if fake.count() != 0 {
		t.Fatalf("expected the receipt to be reconciled without a remote call, got %d calls", fake.count())
	}
	got, err := store.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrackerLog[0].Status != "posted" || got.TrackerLog[0].CommentID != "already-posted-id" {
		t.Fatalf("entry not reconciled from receipt: %+v", got.TrackerLog[0])
	}
}

func TestDelivererPermanentFailureDoesNotWedgeLaterEntries(t *testing.T) {
	store, run := delivererFixture(t)
	run.TrackerLog = []workflow.TrackerLogEntry{
		{ID: "tlog_1", Kind: workflow.TrackerKindNeedsAttention, Revision: run.CurrentRevision, Detail: "first", Status: "pending"},
		{ID: "tlog_2", Kind: workflow.TrackerKindNeedsAttention, Revision: run.CurrentRevision, Detail: "second", Status: "pending"},
	}
	ctx := context.Background()
	run, err := store.Create(ctx, "op-create", nil, run)
	if err != nil {
		t.Fatal(err)
	}
	fail := &fakeTracker{fail: maxTrackerAttempts + 5}
	clock := time.Now()
	d := &Deliverer{Store: store, Now: func() time.Time { return clock }, newTracker: func(workflow.TrackerConfig) (Tracker, error) { return fail, nil }}

	for i := 0; i < maxTrackerAttempts+2; i++ {
		d.tick(ctx)
		clock = clock.Add(time.Hour)
	}
	got, err := store.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrackerLog[0].Status != "failed" {
		t.Fatalf("first entry should be permanently failed: %+v", got.TrackerLog[0])
	}
	// The second entry must still get its turn on a later tick, once the
	// first is terminal.
	succeed := &fakeTracker{}
	d.newTracker = func(workflow.TrackerConfig) (Tracker, error) { return succeed, nil }
	d.tick(ctx)
	got, err = store.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.TrackerLog[1].Status != "posted" {
		t.Fatalf("second entry was blocked by the first's permanent failure: %+v", got.TrackerLog[1])
	}
}

func TestDelivererFailureNeverMutatesRunStateBeyondTheEntry(t *testing.T) {
	store, run := delivererFixture(t)
	run.TrackerLog = []workflow.TrackerLogEntry{{ID: "tlog_1", Kind: workflow.TrackerKindNeedsAttention, Revision: run.CurrentRevision, Detail: "x", Status: "pending"}}
	ctx := context.Background()
	run, err := store.Create(ctx, "op-create", nil, run)
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	fail := &fakeTracker{fail: 100}
	d := &Deliverer{Store: store, newTracker: func(workflow.TrackerConfig) (Tracker, error) { return fail, nil }}
	d.tick(ctx)
	after, err := store.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.CurrentRevision != before.CurrentRevision || after.Current().State != before.Current().State {
		t.Fatal("a tracker failure mutated run or revision state")
	}
	if after.TrackerLog[0].Status != "pending" || after.TrackerLog[0].Attempts != 1 {
		t.Fatalf("unexpected entry after failure: %+v", after.TrackerLog[0])
	}
}
