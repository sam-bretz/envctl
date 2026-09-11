package runstore

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestDrainingEventsKeepTheirRevisionAndReceiptScope(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := newRun(t)
	old := r.CurrentRevision
	if _, err = s.Create(ctx, "create", nil, r); err != nil {
		t.Fatal(err)
	}
	r, err = s.Mutate(ctx, r.ID, r.Version, "rewind", "run.rewound", nil, func(r *workflow.Run) error { _, err := r.Rewind("task", "", nil, time.Now()); return err })
	if err != nil {
		t.Fatal(err)
	}
	version := r.Version
	fn := func(r *workflow.Run) error { r.Revision(old).Runtime.State = "released"; return nil }
	updated, err := s.MutateRevision(ctx, r.ID, old, version, "archive", "checkpoint.archived", nil, fn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.MutateRevision(ctx, r.ID, old, version, "archive", "checkpoint.archived", nil, func(*workflow.Run) error { t.Fatal("receipt reran"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err = s.MutateRevision(ctx, r.ID, r.CurrentRevision, version, "archive", "checkpoint.archived", nil, fn); !errors.Is(err, ErrOperationReuse) {
		t.Fatalf("scope reuse: %v", err)
	}
	events, err := s.Events(ctx, r.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	last := events[len(events)-1]
	if last.Revision != old || last.Version != updated.Version || updated.CurrentRevision != r.CurrentRevision {
		t.Fatalf("incorrect event scope: %#v", last)
	}
}
