package localexec

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type restartProof struct {
	Runtime        string   `json:"runtime"`
	DaemonID       string   `json:"daemon_id"`
	Ready          bool     `json:"ready"`
	Restarted      bool     `json:"vm_restarted_and_restored"`
	RestoreCrash   bool     `json:"restore_crash_reconnected"`
	RestoreJob     string   `json:"restore_job,omitempty"`
	TerminalJobs   []string `json:"terminal_recovery_jobs,omitempty"`
	TerminalRetry  bool     `json:"terminal_recovery_generation"`
	SourceFixtures bool     `json:"partial_worktree_states"`
	KilledAssigns  int      `json:"killed_source_restores"`
	Cleaned        bool     `json:"cleaned"`
}

// One dedicated VM, created and destroyed by this test. It never adopts or
// touches another runtime. Model jobs are not run; harness installation is part
// of production preparation.
func TestRealGuestRestartAndRestoreCrashRecovery(t *testing.T) {
	if os.Getenv("ENVCTL_RESTART_RECOVERY_TEST") != "1" {
		t.Skip("opt-in dedicated VM restart and restore-crash acceptance")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Minute)
	defer cancel()
	b, a := fixture(t)
	dir := filepath.Join(os.TempDir(), "envctl-restart-"+workflow.ID("acceptance"))
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recreate := func() *Backend {
		next := New(store)
		next.Provider = vm.NewLima(dir)
		next.Capabilities = nil
		return next
	}
	b = recreate()
	t.Log("retained restart/restore acceptance state:", dir)
	proof := restartProof{}
	save := func() {
		t.Helper()
		if err := atomicJSON(filepath.Join(dir, "restart-proof.json"), proof); err != nil {
			t.Fatal(err)
		}
	}

	root := t.TempDir()
	compose := `services:
  db:
    image: postgres@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94
    environment: {POSTGRES_HOST_AUTH_METHOD: trust}
    volumes: [data:/var/lib/postgresql/data]
    healthcheck:
      test: [CMD, pg_isready, -U, postgres]
      interval: 1s
      timeout: 1s
      retries: 60
volumes: {data: {}}
`
	for name, body := range map[string]string{"compose.yaml": compose, "work.txt": "baseline\n", "seed.sql": "CREATE TABLE invoices(amount integer); INSERT INTO invoices VALUES (10),(20);"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, root, "init", "-q")
	fixtureGit(t, root, "add", ".")
	fixtureGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "base")
	c := &a.Revision.Config
	c.Repositories = []workflow.Repository{{ID: "app", URL: root, Ref: "HEAD"}}
	c.Runtime.MemoryGiB = 2
	c.Stack.Files = []string{"compose.yaml"}
	// The sleep holds every verification open long enough to observe (and cut)
	// a live guest restore job from the coordinator side.
	c.Data = workflow.DataConfig{Datasets: []workflow.Dataset{{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "app", SeedFile: "seed.sql", VerifySQL: "SELECT (SELECT count(*) FROM invoices WHERE amount < 100) > 0 FROM pg_sleep(4)", VerifyEquals: "t"}}}
	source, archive, err := (repository.Cache{Dir: filepath.Join(dir, "source-cache")}).Prepare(ctx, repository.Resolver{Dir: filepath.Join(dir, "baseline")}, c.Repositories[0])
	if err != nil {
		t.Fatal(err)
	}
	pin := source.Repository.BaseSHA
	a.Revision.SourcePins = map[string]string{"app": pin}
	a.Revision.Checkpoints = map[string]workflow.Checkpoint{}
	runtimeID := "envctl-" + strings.ReplaceAll(a.Revision.ID, "_", "-")
	a.Revision.Runtime = workflow.RuntimeState{ID: runtimeID, Provider: "lima", Location: "local", State: "preparing"}
	proof.Runtime = runtimeID
	save()
	success := false
	t.Cleanup(func() {
		if !success {
			t.Log("failure retains the VM and state for diagnosis:", runtimeID, dir)
			return
		}
		clean, stop := context.WithTimeout(context.Background(), 3*time.Minute)
		defer stop()
		if err := b.Release(clean, a); err != nil {
			t.Error("owned VM cleanup:", err)
			return
		}
		if _, err := b.Provider.Inspect(clean, runtimeID); !errors.Is(err, vm.ErrMissing) {
			t.Error("owned VM still present after cleanup", err)
			return
		}
		proof.Cleaned = true
		save()
	})

	guest := func(script string, args ...string) string {
		t.Helper()
		var out, diagnostic bytes.Buffer
		if err := b.Provider.Exec(ctx, runtimeID, vm.Command{Args: append([]string{"sudo", "bash", "-c", script, "envctl-acceptance"}, args...), Stdout: &out, Stderr: &diagnostic}); err != nil {
			t.Fatalf("guest command failed: %v: %s", err, diagnostic.String())
		}
		return strings.TrimSpace(out.String())
	}
	sql := func(query string) string {
		t.Helper()
		p, err := b.currentStack(a)
		if err != nil {
			t.Fatal(err)
		}
		container, err := b.stack(a).Container(ctx, p, "db")
		if err != nil {
			t.Fatal(err)
		}
		return guest(`docker exec -i "$1" psql -U postgres -d billing -qtA -v ON_ERROR_STOP=1 -c "$2"`, container, query)
	}
	dataJobs := func() []string {
		t.Helper()
		out := guest(`ls -1 /var/lib/envctl/jobs 2>/dev/null | grep '^data_' || true`)
		if out == "" {
			return nil
		}
		return strings.Fields(out)
	}
	newJobs := func(before []string) []string {
		var result []string
		for _, id := range dataJobs() {
			if !slices.Contains(before, id) {
				result = append(result, id)
			}
		}
		return result
	}
	jobState := func(id string) string {
		t.Helper()
		status, err := b.guest(a).Poll(ctx, id, 0)
		if err != nil {
			t.Fatal(err)
		}
		return status.State
	}
	// Wait until a new data job is live in the guest; return its ID.
	liveJob := func(before []string, done <-chan error) string {
		t.Helper()
		for ctx.Err() == nil {
			select {
			case err := <-done:
				t.Fatal("operation ended before its restore job was observed live", err)
			default:
			}
			for _, id := range newJobs(before) {
				if jobState(id) == "running" {
					return id
				}
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatal("no live restore job observed")
		return ""
	}
	ownInstances := func() int {
		t.Helper()
		// Only this test's reservation directory may own this runtime name.
		entries, err := os.ReadDir(filepath.Join(dir, "runtimes"))
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}
	capabilities := []string{"repositories.readwrite", "runtime.compose", "dataset.restore"}
	ready := func(backend *Backend) {
		t.Helper()
		probes, err := backend.readiness(ctx, a, capabilities)
		if err != nil {
			t.Fatal("readiness", err)
		}
		for _, probe := range probes {
			if !probe.Passed {
				t.Fatal("readiness failed:", probe.Capability, probe.Detail)
			}
		}
	}

	// Production preparation: dedicated VM, guest runner, source import, harness.
	prepared, err := b.Prepare(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	a.Revision.Runtime, a.Revision.SourcePins = prepared.Runtime, prepared.SourcePins
	proof.DaemonID = prepared.Runtime.DaemonID
	save()
	ready(b)
	proof.Ready = true
	save()
	sql("INSERT INTO invoices VALUES (777)")
	if got := sql("SELECT count(*) FROM invoices WHERE amount = 777"); got != "1" {
		t.Fatal("marker row not written", got)
	}

	// M2: the VM stops underneath an active revision. A recreated coordinator's
	// readiness path restarts the same VM, keeps its Docker identity, restores
	// the Compose stack and keeps volume data.
	if err = b.Provider.Stop(ctx, runtimeID); err != nil {
		t.Fatal(err)
	}
	if inst, err := b.Provider.Inspect(ctx, runtimeID); err != nil || inst.State == "running" {
		t.Fatal("VM did not stop", inst, err)
	}
	b = recreate()
	ready(b)
	if inst, err := b.Provider.Inspect(ctx, runtimeID); err != nil || inst.State != "running" {
		t.Fatal("VM was not restarted", inst, err)
	}
	if id := guest(`docker info --format '{{.ID}}'`); id != proof.DaemonID {
		t.Fatal("Docker daemon identity changed across restart", id)
	}
	if got := sql("SELECT count(*) FROM invoices WHERE amount = 777"); got != "1" {
		t.Fatal("volume data lost across VM restart", got)
	}
	if n := ownInstances(); n != 1 {
		t.Fatal("restart allocated another VM reservation", n)
	}
	proof.Restarted = true
	save()
	t.Log("VM restart restored the same runtime, daemon identity, healthy stack and volume data")

	code := func() engine.Assignment {
		next := workflow.Clone(a)
		next.Attempt = workflow.Attempt{ID: workflow.ID("attempt"), Node: "code", State: "running", Inputs: map[string]string{}}
		return next
	}

	// M5(a): the coordinator dies while a real PostgreSQL restore job for a new
	// attempt is live. Its guest job keeps running; the recreated coordinator
	// reconnects to it and never starts a second restore.
	first := code()
	before := dataJobs()
	dying, die := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- b.Start(dying, first) }()
	job := liveJob(before, done)
	die()
	if err = <-done; err == nil {
		t.Fatal("interrupted coordinator reported a completed Start")
	}
	if state := jobState(job); state != "running" && state != "completed" {
		t.Fatal("coordinator death stopped the guest restore job", state)
	}
	b = recreate()
	if err = b.Start(ctx, first); err != nil {
		t.Fatal("recreated coordinator could not reconcile the restore", err)
	}
	if jobs := newJobs(before); len(jobs) != 1 || jobs[0] != job || jobState(job) != "completed" {
		t.Fatal("restore was repeated instead of reconnected", jobs)
	}
	if _, err = b.load(first); err != nil {
		t.Fatal("attempt was not frozen after reconciliation", err)
	}
	if b.dataBusy(first) {
		t.Fatal("writer quiescence was not released after the restore")
	}
	if got := sql("SELECT count(*) FROM invoices WHERE amount = 777"); got != "0" {
		t.Fatal("restored dataset does not match the retained snapshot", got)
	}
	proof.RestoreCrash, proof.RestoreJob = true, job
	save()
	t.Log("coordinator death during a live restore reconnected to exactly one guest job:", job)

	// M5(c): the restore job itself dies (process group killed). That terminal
	// journal cannot be retried under its ID; a durable new generation runs
	// after backoff and verifies the restore.
	second := code()
	before = dataJobs()
	done = make(chan error, 1)
	go func() { done <- b.Start(ctx, second) }()
	killed := liveJob(before, done)
	guest(`systemctl kill --signal=SIGKILL "envctl-job-$1.service"`, killed)
	err = <-done
	var terminal *checkpoint.TerminalFailure
	if !errors.As(err, &terminal) {
		t.Fatal("killed restore job was not reported as terminal", err)
	}
	record, _, err := b.readDataRecovery(second, second.Attempt.ID+"_restore_billing")
	if err != nil || record.Generation != 1 || len(record.Failures) != 1 {
		t.Fatal("terminal restore did not record a recovery generation", record, err)
	}
	b = recreate()
	for {
		err = b.Start(ctx, second)
		if !errors.Is(err, errDataRecoveryBackoff) {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		t.Fatal("next recovery generation failed", err)
	}
	jobs := newJobs(before)
	if len(jobs) != 2 || !slices.Contains(jobs, killed) {
		t.Fatal("expected the killed job plus exactly one recovery generation", jobs)
	}
	for _, id := range jobs {
		if want := map[bool]string{true: "interrupted", false: "completed"}[id == killed]; jobState(id) != want {
			t.Fatal("unexpected recovery job state", id, jobState(id))
		}
	}
	proof.TerminalJobs, proof.TerminalRetry = jobs, true
	save()
	t.Log("killed restore job recovered through one new generation:", jobs)

	// M5(b): partial worktree states a dead coordinator can leave behind, then
	// real SSH-session kills during bundle/companion restore and assignment.
	repos := b.repos(a)
	sourceDir, err := repos.SourceDir("app")
	if err != nil {
		t.Fatal(err)
	}
	verifyWorktree := func(r repository.Guest, attempt, commit, want string) {
		t.Helper()
		dest, err := r.WorktreeDir(attempt, "app")
		if err != nil {
			t.Fatal(err)
		}
		src, _ := r.SourceDir("app")
		out := guest(`set -e
cd "$1"
test "$(sudo -u envctl-agent git rev-parse HEAD)" = "$2"
test -z "$(sudo -u envctl-agent git status --porcelain)"
test "$(sudo -u envctl-agent git symbolic-ref --short HEAD)" = "$4"
test -f "/var/lib/envctl/repository-receipts/$5/$6/app"
cat work.txt
echo "worktrees=$(sudo -u envctl-agent git -C "$3" worktree list --porcelain | grep -cx "worktree $1")"`, dest, commit, src, "envctl/"+r.Revision+"/"+attempt+"/app", r.Revision, attempt)
		if !strings.HasPrefix(out, want) || !strings.HasSuffix(out, "worktrees=1") {
			t.Fatalf("assignment did not converge to exact content: %q", out)
		}
	}
	partial := func(attempt string, removeDir bool) {
		t.Helper()
		dest, err := repos.WorktreeDir(attempt, "app")
		if err != nil {
			t.Fatal(err)
		}
		guest(`set -e
install -d -o envctl-agent -g envctl-agent -m 700 "$(dirname "$2")"
sudo -u envctl-agent git -C "$1" worktree add --no-checkout --detach --lock --reason initializing "$2" "$3" >/dev/null 2>&1
sudo -u envctl-agent git -C "$1" branch "$4" "$3"
if [ "$5" = yes ]; then rm -rf -- "$2"; fi`, sourceDir, dest, pin, "envctl/"+a.Revision.ID+"/"+attempt+"/app", map[bool]string{true: "yes", false: "no"}[removeDir])
	}
	for _, removeDir := range []bool{false, true} {
		attempt := workflow.ID("attempt")
		partial(attempt, removeDir)
		if _, err = repos.Assign(ctx, attempt, "app", pin); err != nil {
			t.Fatal("partial locked worktree did not converge", removeDir, err)
		}
		verifyWorktree(repos, attempt, pin, "baseline")
	}
	// A receipted worktree keeps a live writer's edits on replay.
	writer := workflow.ID("attempt")
	dest, err := repos.Assign(ctx, writer, "app", pin)
	if err != nil {
		t.Fatal(err)
	}
	guest(`printf 'edited\n' | sudo -u envctl-agent tee "$1/work.txt" >/dev/null`, dest)
	if _, err = repos.Assign(ctx, writer, "app", pin); err != nil || guest(`cat "$1/work.txt"`, dest) != "edited" {
		t.Fatal("replay reset a receipted writer's edits", err)
	}
	commit, err := repos.Capture(ctx, writer, "app", true)
	if err != nil {
		t.Fatal(err)
	}
	var bundle, objects bytes.Buffer
	if err = repos.ExportBundle(ctx, writer, "app", commit, &bundle); err != nil {
		t.Fatal(err)
	}
	if err = repos.ExportObjects(ctx, writer, "app", commit, &objects); err != nil {
		t.Fatal(err)
	}
	proof.SourceFixtures = true
	save()
	// A new revision in this VM restores the checkpoint from retained bytes.
	// Each attempt's coordinator is killed at a different point; replay must
	// converge to the exact commit with one worktree per assignment.
	restored := repository.Guest{Executor: b.Provider, Runtime: runtimeID, Revision: workflow.ID("rev")}
	if err = restored.Import(ctx, source, archive); err != nil {
		t.Fatal(err)
	}
	restore := func(ctx context.Context, attempt string) error {
		if err := restored.RestoreBundle(ctx, "app", commit, bytes.NewReader(bundle.Bytes())); err != nil {
			return err
		}
		if err := restored.RestoreObjects(ctx, "app", commit, bytes.NewReader(objects.Bytes())); err != nil {
			return err
		}
		_, err := restored.Assign(ctx, attempt, "app", commit)
		return err
	}
	for _, delay := range []time.Duration{200 * time.Millisecond, 500 * time.Millisecond, 900 * time.Millisecond, 1400 * time.Millisecond, 2 * time.Second} {
		attempt := workflow.ID("attempt")
		cut, stop := context.WithTimeout(ctx, delay)
		if err := restore(cut, attempt); err != nil {
			proof.KilledAssigns++
		}
		stop()
		if err = restore(ctx, attempt); err != nil {
			t.Fatal("replay after coordinator death did not converge", delay, err)
		}
		verifyWorktree(restored, attempt, commit, "edited")
	}
	if proof.KilledAssigns == 0 {
		t.Fatal("no source restoration was actually interrupted; crash points not exercised")
	}
	save()
	t.Log("partial worktrees and", proof.KilledAssigns, "killed source restorations converged to exact content")
	success = true
}
