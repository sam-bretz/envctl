package localexec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/repository"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealDatasetReadinessStageRestoreAndQuiescence(t *testing.T) {
	if os.Getenv("ENVCTL_DATA_TEST") != "1" {
		t.Skip("opt-in production dataset integration in an owned guest")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("explicit owned acceptance VM required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	b, a := fixture(t)
	b.Provider = vm.NewLima(state)
	a.Revision.Runtime.ID = name
	source := t.TempDir()
	compose := `services:
  db:
    image: postgres@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94
    environment: {POSTGRES_HOST_AUTH_METHOD: trust}
    volumes: [data:/var/lib/postgresql/data]
    healthcheck:
      test: [CMD, pg_isready, -U, postgres]
      interval: 1s
      timeout: 1s
      retries: 30
  writer:
    image: busybox:1.37.0
    command: [sleep, '3600']
volumes: {data: {}}
`
	for file, body := range map[string]string{"compose.yaml": compose, "seed.sql": "CREATE TABLE invoices(amount integer); INSERT INTO invoices VALUES (10),(20);"} {
		if err := os.WriteFile(filepath.Join(source, file), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, source, "init", "-q")
	fixtureGit(t, source, "add", ".")
	fixtureGit(t, source, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "base")
	a.Revision.Config.Repositories = []workflow.Repository{{ID: "app", URL: source, Ref: "HEAD"}}
	a.Revision.Config.Stack.Files = []string{"compose.yaml"}
	a.Revision.Config.Data = workflow.DataConfig{Quiesce: []string{"writer"}, Datasets: []workflow.Dataset{{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "app", SeedFile: "seed.sql", VerifySQL: "SELECT count(*) > 0 FROM invoices", VerifyEquals: "t"}}}
	resolved, err := (repository.Resolver{Dir: filepath.Join(b.Store.Dir, "source")}).Prepare(ctx, a.Revision.Config.Repositories[0])
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(b.Store.Dir, "source.tar")
	if err = repository.Archive(ctx, resolved, archive); err != nil {
		t.Fatal(err)
	}
	if err = b.repos(a).Import(ctx, resolved, archive); err != nil {
		t.Fatal(err)
	}
	a.Revision.SourcePins = map[string]string{"app": resolved.Repository.BaseSHA}
	dirs, err := b.worktrees(ctx, a, "readiness", a.Revision.SourcePins)
	if err != nil {
		t.Fatal(err)
	}
	p, err := b.stack(a).Prepare(ctx, stackSpec(a, a.Revision.ID+"_setup", dirs))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		current, e := b.currentStack(a)
		if e == nil {
			if e = b.stack(a).Down(clean, current, true); e != nil {
				t.Error(e)
			}
		}
		if e = b.Provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", "/work/envctl/datasets/" + a.Revision.ID, "/work/envctl/revisions/" + a.Revision.ID, "/var/lib/envctl/repository-receipts/" + a.Revision.ID}}); e != nil {
			t.Error(e)
		}
	})
	if err = b.stack(a).Up(ctx, p, 60); err != nil {
		t.Fatal(err)
	}
	if err = b.saveStack(a, p); err != nil {
		t.Fatal(err)
	}
	// Changing the checkout's seed after pin resolution must not change the
	// SQL used to seed the run.
	if err = b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "tee", dirs["app"] + "/seed.sql"}, Stdin: strings.NewReader("invalid replacement SQL")}); err != nil {
		t.Fatal(err)
	}
	passed, detail, err := b.datasetReadiness(ctx, a)
	if err != nil || !passed {
		t.Fatal(detail, err)
	}
	if b.dataBusy(a) {
		t.Fatal("baseline left writers stopped")
	}
	query := func(sql string) string {
		t.Helper()
		current, err := b.currentStack(a)
		if err != nil {
			t.Fatal(err)
		}
		container, err := b.stack(a).Container(ctx, current, "db")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err = b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "docker", "exec", "-i", container, "psql", "-U", "postgres", "-d", "billing", "-At", "-v", "ON_ERROR_STOP=1"}, Stdin: strings.NewReader(sql), Stdout: &out}); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}
	if query("SELECT sum(amount) FROM invoices") != "30" {
		t.Fatal("seed did not use pinned content")
	}
	if err = b.beginData(ctx, a, p, "interrupted_capture"); err != nil {
		t.Fatal(err)
	}
	b = New(b.Store)
	b.Provider = vm.NewLima(state)
	if passed, _, err = b.datasetReadiness(ctx, a); err != nil || passed {
		t.Fatal("interrupted capture did not retain readiness hold", err)
	}
	entries, err := b.stack(a).Status(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Service == "writer" && entry.State == "running" {
			t.Fatal("readiness restarted a quiesced writer")
		}
	}
	if err = b.beginData(ctx, a, p, "interrupted_capture"); err != nil {
		t.Fatal(err)
	}
	if err = b.endData(ctx, a, p); err != nil {
		t.Fatal(err)
	}
	if err = b.Start(ctx, a); err != nil {
		t.Fatal("Code input restore", err)
	}
	var scratchOwner bytes.Buffer
	if err = b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "stat", "-c", "%U:%G:%a", scratchDirectory(a)}, Stdout: &scratchOwner}); err != nil || strings.TrimSpace(scratchOwner.String()) != "envctl-agent:envctl-agent:700" {
		t.Fatal("attempt scratch directory is not private and agent-owned", err)
	}
	query("INSERT INTO invoices VALUES (10)")
	r, err := b.load(a)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.captureData(ctx, a, r); err != nil {
		t.Fatal("stage capture", err)
	}
	if r.Result.DatasetDigest != workflow.Digest(r.Result.Datasets) || len(r.Result.Datasets) != 1 {
		t.Fatal("stage manifest incomplete")
	}
	a.Revision.Checkpoints["code"] = workflow.Checkpoint{ID: "cp_code", Result: workflow.Result{Commits: workflow.Clone(a.Revision.SourcePins), Datasets: workflow.Clone(r.Result.Datasets), DatasetDigest: r.Result.DatasetDigest}}
	query("INSERT INTO invoices VALUES (999); CREATE TABLE unwanted(id int)")
	a.Attempt = workflow.Attempt{ID: workflow.ID("attempt"), Node: "qa", Inputs: map[string]string{"code": "cp_code"}}
	if err = b.Start(ctx, a); err != nil {
		t.Fatal("QA data input restore", err)
	}
	if query("SELECT sum(amount) FROM invoices") != "40" || query("SELECT count(*) FROM pg_tables WHERE tablename='unwanted'") != "0" {
		t.Fatal("QA did not restore its exact Code dataset")
	}
	if b.dataBusy(a) {
		t.Fatal("completed restoration retained quiescence ownership")
	}
	t.Log("pinned seeding/restore readiness, durable writer quiescence, backend recreation, stage data capture and exact predecessor restoration passed")
}
