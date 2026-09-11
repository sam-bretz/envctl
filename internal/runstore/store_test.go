package runstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func newRun(t *testing.T) *workflow.Run {
	t.Helper()
	c, err := workflow.Parse([]byte("version: 2\nproject: test\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := workflow.NewRun("demo", "implement feature", "dev", c, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestTransactionalReceiptsSurviveRestart(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	r := newRun(t)
	if _, err = s.Create(ctx, "create", map[string]string{"task": "feature"}, r); err != nil {
		t.Fatal(err)
	}
	changed, err := s.Mutate(ctx, r.ID, 1, "priority", "run.priority", 7, func(r *workflow.Run) error { r.Priority = 7; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if changed.Version != 2 {
		t.Fatal(changed.Version)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get(ctx, r.ID)
	if err != nil || got.Priority != 7 {
		t.Fatalf("restore: %+v %v", got, err)
	}
	called := false
	again, err := s.Mutate(ctx, r.ID, 1, "priority", "run.priority", 7, func(r *workflow.Run) error { called = true; return nil })
	if err != nil || called || again.Version != 2 {
		t.Fatal("receipt was not replayed", err)
	}
	if _, err = s.Mutate(ctx, r.ID, 2, "priority", "run.priority", 8, func(r *workflow.Run) error { return nil }); !errors.Is(err, ErrOperationReuse) {
		t.Fatalf("operation reuse: %v", err)
	}
	if _, err = s.Mutate(ctx, r.ID, 1, "stale", "run.priority", 9, func(r *workflow.Run) error { return nil }); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale update: %v", err)
	}
	events, err := s.Events(ctx, r.ID, 0, 100)
	if err != nil || len(events) != 2 {
		t.Fatalf("events: %+v %v", events, err)
	}
	after, err := s.Events(ctx, r.ID, events[0].Sequence, 100)
	if err != nil || len(after) != 1 {
		t.Fatal("resume cursor", err)
	}
}

func TestPublicationGuardSerializesRewind(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := newRun(t)
	r.Current().State = "active"
	if _, err = s.Create(ctx, "create", "fixture", r); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := s.GuardedEffect(ctx, r.ID, r.CurrentRevision, "publish", "publication", func(current *workflow.Run) error {
			close(entered)
			<-release
			current.Priority = 10 // Stands for the broker's recorded receipt.
			return nil
		})
		done <- err
	}()
	<-entered
	waitCtx, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	called := false
	_, err = s.Mutate(waitCtx, r.ID, r.Version, "rewind-blocked", "rewind", nil, func(current *workflow.Run) error {
		called = true
		_, err := current.Rewind("task", "", nil, time.Now())
		return err
	})
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("rewind crossed in-flight publication: %v", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	latest, err := s.Get(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if latest.Priority != 10 {
		t.Fatal("publication receipt lost")
	}
	updated, err := s.Mutate(ctx, r.ID, latest.Version, "rewind", "rewind", nil, func(current *workflow.Run) error { _, err := current.Rewind("task", "", nil, time.Now()); return err })
	if err != nil || updated.CurrentRevision == r.CurrentRevision {
		t.Fatal("rewind after publication failed", err)
	}
	_, err = s.GuardedEffect(ctx, r.ID, r.CurrentRevision, "stale-publish", "publication", func(*workflow.Run) error { t.Fatal("stale revision published"); return nil })
	if !errors.Is(err, workflow.ErrConflict) {
		t.Fatal("stale publication not fenced", err)
	}
}
func TestFailureRollsBackAndAllowsRetry(t *testing.T) {
	ctx := context.Background()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	r := newRun(t)
	if _, err = s.Create(ctx, "create", r.Description, r); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Mutate(ctx, r.ID, 1, "op", "run.priority", 5, func(r *workflow.Run) error { r.Priority = 5; return errors.New("rejected") }); err == nil {
		t.Fatal("mutation succeeded")
	}
	got, _ := s.Get(ctx, r.ID)
	if got.Priority != 0 || got.Version != 1 {
		t.Fatal("failed mutation leaked")
	}
	if _, err = s.Mutate(ctx, r.ID, 1, "op", "run.priority", 5, func(r *workflow.Run) error { r.Priority = 5; return nil }); err != nil {
		t.Fatal(err)
	}
}
func TestConcurrentClientsCannotOverwrite(t *testing.T) {
	dir := t.TempDir()
	s1, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s1.Close()
	s2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	ctx := context.Background()
	r := newRun(t)
	if _, err = s1.Create(ctx, "create", r.Description, r); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i, s := range []*Store{s1, s2} {
		wg.Add(1)
		go func(i int, s *Store) {
			defer wg.Done()
			_, err := s.Mutate(ctx, r.ID, 1, workflow.ID("op"), "run.priority", i, func(r *workflow.Run) error { r.Priority = i; return nil })
			results <- err
		}(i, s)
	}
	wg.Wait()
	close(results)
	good, conflict := 0, 0
	for err := range results {
		if err == nil {
			good++
		} else if errors.Is(err, workflow.ErrConflict) {
			conflict++
		} else {
			t.Fatal(err)
		}
	}
	if good != 1 || conflict != 1 {
		t.Fatalf("successes %d conflicts %d", good, conflict)
	}
}
func TestArtifactsAreContentAddressedAndVerified(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	a, err := s.PutArtifact("plan", "text/markdown", []byte("the plan"))
	if err != nil {
		t.Fatal(err)
	}
	data, err := s.Artifact(a.Digest)
	if err != nil || string(data) != "the plan" {
		t.Fatal(err)
	}
	if _, err = s.Artifact("../../runs.db"); err == nil {
		t.Fatal("path traversal accepted")
	}
	if err = os.WriteFile(filepath.Join(s.Dir, "artifacts", a.Digest[:2], a.Digest), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Artifact(a.Digest); err == nil {
		t.Fatal("corrupt artifact trusted")
	}
}
