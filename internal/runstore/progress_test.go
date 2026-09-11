package runstore

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestProgressIsDisplayStateOutsideTheVersionedRun(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := newRun(t)
	if _, err = s.Create(ctx, "create", "progress", r); err != nil {
		t.Fatal(err)
	}
	r, err = s.Mutate(ctx, r.ID, r.Version, "attempts", "test.attempts", nil, func(run *workflow.Run) error {
		v := run.Current()
		v.Attempts = append(v.Attempts, workflow.Attempt{ID: "attempt_live", Node: "task", State: "running"}, workflow.Attempt{ID: "attempt_done", Node: "task", State: "failed"})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"attempt_live", "attempt_done"} {
		if err = s.SetProgress(ctx, r.ID, id, workflow.Progress{Phase: "worker", Activity: []string{"agent: " + id}, UpdatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != r.Version {
		t.Fatal("progress changed the run version")
	}
	if p := got.Current().Attempt("attempt_live").Progress; p == nil || p.Activity[0] != "agent: attempt_live" {
		t.Fatal("running attempt has no progress", p)
	}
	if got.Current().Attempt("attempt_done").Progress != nil {
		t.Fatal("terminal attempt shows stale progress")
	}
	listed, err := s.List(ctx)
	if err != nil || len(listed) != 1 || listed[0].Current().Attempt("attempt_live").Progress == nil {
		t.Fatal("listing omits progress", err)
	}
	// A snapshot read with its overlay and written back (as GuardedEffect
	// does) must not persist progress into the versioned document.
	if _, err = s.Mutate(ctx, r.ID, got.Version, "writeback", "test.writeback", nil, func(run *workflow.Run) error {
		*run = *workflow.Clone(got)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var body []byte
	if err = s.db.QueryRowContext(ctx, "SELECT body FROM runs WHERE id=?", r.ID).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(body, []byte(`"progress"`)) {
		t.Fatal("overlaid progress was persisted into the run document")
	}
}
