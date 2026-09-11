package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// fakeGuest follows the guest journal contract: a job ID starts at most once,
// resubmitting the identical request observes it, and its state is scripted.
type fakeGuest struct {
	mu        sync.Mutex
	scripts   map[int][]string // generation-ordered job states, by submission order of new IDs
	jobs      map[string]*fakeJob
	order     []string
	transport error
	onPoll    func(id, state string)
}
type fakeJob struct {
	starts, polls int
	states        []string
}

func (f *fakeGuest) Exec(ctx context.Context, _ string, c vm.Command) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	args := strings.Join(c.Args, " ")
	switch {
	case strings.Contains(args, " ps --quiet "):
		_, err := io.WriteString(c.Stdout, strings.Repeat("a", 64)+"\n")
		return err
	case len(c.Args) > 2 && c.Args[1] == "python3" && c.Args[2] == "-c":
		if c.Stdin != nil {
			_, _ = io.Copy(io.Discard, c.Stdin)
		}
		return nil
	case len(c.Args) == 4 && c.Args[2] == guestjob.RunnerPath:
		f.mu.Lock()
		defer f.mu.Unlock()
		var req struct {
			ID     string
			Cursor int64
		}
		if err := json.NewDecoder(c.Stdin).Decode(&req); err != nil {
			return err
		}
		job := f.jobs[req.ID]
		if c.Args[3] == "submit" && job == nil {
			job = &fakeJob{starts: 1, states: f.scripts[len(f.order)]}
			f.jobs[req.ID] = job
			f.order = append(f.order, req.ID)
		}
		if job == nil {
			return json.NewEncoder(c.Stdout).Encode(guestjob.Status{ID: req.ID, State: "missing"})
		}
		if c.Args[3] == "status" && f.transport != nil {
			return f.transport
		}
		state := job.states[min(job.polls, len(job.states)-1)]
		if c.Args[3] == "status" {
			job.polls++
		}
		if f.onPoll != nil {
			f.onPoll(req.ID, state)
		}
		status := guestjob.Status{ID: req.ID, State: state, Cursor: req.Cursor}
		if output := `{"ok":true,"version":"fixture 1","detail":"verified"}`; state == "completed" && req.Cursor < int64(len(output)) {
			status.Output, status.Cursor = output[req.Cursor:], int64(len(output))
		}
		return json.NewEncoder(c.Stdout).Encode(status)
	}
	return errors.New("unexpected guest command: " + args)
}

func restoreFixture(t *testing.T, adapter string) (Client, workflow.Dataset, workflow.DatasetSnapshot, *fakeGuest) {
	t.Helper()
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	spec := workflow.Dataset{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "app", SeedFile: "seed.sql", VerifySQL: "SELECT 1", VerifyEquals: "1"}
	if adapter == "http-fixture" {
		spec = workflow.Dataset{ID: "external", Adapter: "http-fixture", Service: "db", SeedRepository: "app", SeedFile: "seed.json", VerifyKey: "tax-rate", VerifyEquals: `{"rate":0.2}`}
	}
	source, err := store.PutArtifact("dataset."+spec.ID, spec.SnapshotMedia(), []byte("retained snapshot bytes"))
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := store.PutArtifact("dataset."+spec.ID+".verification", "application/json", []byte(`{"ok":true}`))
	if err != nil {
		t.Fatal(err)
	}
	snapshot := workflow.DatasetSnapshot{Adapter: spec.Adapter, BindingDigest: workflow.Digest(spec), Format: spec.SnapshotFormat(), ToolVersion: "fixture 1", Source: source, Evidence: evidence}
	guest := &fakeGuest{scripts: map[int][]string{}, jobs: map[string]*fakeJob{}}
	prepared := gueststack.Prepared{Spec: gueststack.Spec{ID: "stack", Project: "rev_one", Root: "/work/envctl/revisions/rev_one/attempts/a/app", Stack: manifest.Stack{Files: []string{"compose.yaml"}}}, File: "/var/lib/envctl/stacks/stack/compose.yaml", Services: []string{"db"}}
	c := Client{Guest: guestjob.Client{Provider: guest, Runtime: "envctl-test"}, Stack: gueststack.Client{Executor: guest, Runtime: "envctl-test"}, Prepared: prepared, Store: store, Revision: "rev_one"}
	return c, spec, snapshot, guest
}

func TestRestoreReconnectsToInFlightJobAfterCoordinatorDeath(t *testing.T) {
	for _, adapter := range []string{"postgres", "http-fixture"} {
		t.Run(adapter, func(t *testing.T) {
			c, spec, snapshot, guest := restoreFixture(t, adapter)
			guest.scripts[0] = []string{"running", "running", "running", "completed"}
			ctx, die := context.WithCancel(context.Background())
			guest.onPoll = func(_, state string) {
				if state == "running" {
					die() // coordinator dies while the restore job is live in the guest
				}
			}
			if err := c.Restore(ctx, "attempt_one_restore_"+spec.ID, spec, snapshot); !errors.Is(err, context.Canceled) {
				t.Fatal("coordinator death was not observed as transport loss", err)
			}
			guest.onPoll = nil
			// A recreated coordinator reconnects to the same journal entry.
			if err := c.Restore(context.Background(), "attempt_one_restore_"+spec.ID, spec, snapshot); err != nil {
				t.Fatal(err)
			}
			if len(guest.order) != 1 || guest.jobs[guest.order[0]].starts != 1 {
				t.Fatal("reconnect started a second restore", guest.order)
			}
		})
	}
}

func TestTerminalDataJobsNeedANewGenerationAndTransportLossDoesNot(t *testing.T) {
	for _, adapter := range []string{"postgres", "http-fixture"} {
		t.Run(adapter, func(t *testing.T) {
			c, spec, snapshot, guest := restoreFixture(t, adapter)
			guest.scripts[0] = []string{"interrupted"}
			guest.scripts[1] = []string{"completed"}
			op := "attempt_one_restore_" + spec.ID
			guest.transport = errors.New("ssh: connection lost")
			err := c.Restore(context.Background(), op, spec, snapshot)
			var terminal *TerminalFailure
			if err == nil || errors.As(err, &terminal) {
				t.Fatal("transport loss must stay reconnectable, not terminal", err)
			}
			guest.transport = nil
			err = c.Restore(context.Background(), op, spec, snapshot)
			if !errors.As(err, &terminal) || terminal.State != "interrupted" {
				t.Fatal("interrupted job was not reported as terminal", err)
			}
			// Replay under the same generation observes the same terminal job.
			if err = c.Restore(context.Background(), op, spec, snapshot); !errors.As(err, &terminal) || len(guest.order) != 1 {
				t.Fatal("same generation must not start another job", err, guest.order)
			}
			next := c
			next.Generation = 1
			if err = next.Restore(context.Background(), op, spec, snapshot); err != nil {
				t.Fatal(err)
			}
			if len(guest.order) != 2 || guest.order[0] == guest.order[1] {
				t.Fatal("new generation did not use a distinct job identity", guest.order)
			}
			// Generation zero preserves the identities of existing guest journals.
			legacy := workflow.Digest(struct {
				Revision, Operation, Action, Container, Input, Version string
				Spec                                                   workflow.Dataset
			}{c.Revision, op, "restore", strings.Repeat("a", 64), snapshot.Source.Digest, snapshot.ToolVersion, spec})
			if guest.order[0] != "data_"+legacy[:40] {
				t.Fatal("generation zero changed the job identity")
			}
		})
	}
}

func TestCompletedJobWithoutVerifiedResultIsTerminal(t *testing.T) {
	c, spec, snapshot, guest := restoreFixture(t, "postgres")
	guest.scripts[0] = []string{"failed"}
	var terminal *TerminalFailure
	if err := c.Restore(context.Background(), "attempt_one_restore_billing", spec, snapshot); !errors.As(err, &terminal) || terminal.State != "failed" {
		t.Fatal("failed job was not terminal", err)
	}
}
