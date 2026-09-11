package checkpoint

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const postgresFixtureImage = "postgres@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94"

func TestRealPostgresCheckpointRestoreAndIsolation(t *testing.T) {
	if os.Getenv("ENVCTL_DATA_TEST") != "1" {
		t.Skip("opt-in real PostgreSQL guest stacks")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("select the owned acceptance VM explicitly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	provider := vm.NewLima(state)
	if _, err := provider.Inspect(ctx, name); err != nil {
		t.Fatal(err)
	}
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	makeAdapter := func() Postgres {
		t.Helper()
		revision := workflow.ID("rev")
		root := "/work/envctl/fixtures/" + revision
		compose := fmt.Sprintf("services:\n  db:\n    image: %s\n    environment: {POSTGRES_HOST_AUTH_METHOD: trust}\n    volumes: [data:/var/lib/postgresql/data]\n    healthcheck:\n      test: [CMD, pg_isready, -U, postgres]\n      interval: 1s\n      timeout: 1s\n      retries: 30\nvolumes: {data: {}}\n", postgresFixtureImage)
		if err = provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "mkdir", "-p", root}}); err != nil {
			t.Fatal(err)
		}
		if err = provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "tee", root + "/compose.yaml"}, Stdin: strings.NewReader(compose)}); err != nil {
			t.Fatal(err)
		}
		stack := gueststack.Client{Executor: provider, Runtime: name}
		prepared, err := stack.Prepare(ctx, gueststack.Spec{ID: revision, Project: revision, Root: root, Stack: manifest.Stack{Files: []string{"compose.yaml"}}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			clean, stop := context.WithTimeout(context.Background(), time.Minute)
			defer stop()
			if err := stack.Down(clean, prepared, true); err != nil {
				t.Error(err)
			}
			if err := provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", root, "/work/envctl/datasets/" + revision}}); err != nil {
				t.Error(err)
			}
		})
		if err = stack.Up(ctx, prepared, 60); err != nil {
			t.Fatal(err)
		}
		return Postgres{Guest: guestjob.Client{Provider: provider, Runtime: name}, Stack: stack, Prepared: prepared, Store: store, Revision: revision}
	}
	a, b := makeAdapter(), makeAdapter()
	spec := workflow.Dataset{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "app", SeedFile: "seed.sql", VerifySQL: "SELECT sum(amount) FROM invoices", VerifyEquals: "30"}
	seed, err := store.PutArtifact("seed", "application/sql", []byte("CREATE TABLE invoices(id integer primary key, amount integer); INSERT INTO invoices VALUES (1,10),(2,20);"))
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := a.Seed(ctx, "seed", spec, seed)
	if err != nil {
		t.Fatal("seed", err)
	}
	if snapshot.Source.Digest == "" || snapshot.Evidence.Digest == "" || !strings.Contains(snapshot.ToolVersion, "17.6") {
		t.Fatal("snapshot lacks versioned evidence")
	}
	if err = b.Restore(ctx, "restore", spec, snapshot); err != nil {
		t.Fatal("restore", err)
	}
	query := func(p Postgres, sql string) string {
		t.Helper()
		container, err := p.Stack.Container(ctx, p.Prepared, "db")
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err = provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "docker", "--host", "unix:///var/run/docker.sock", "exec", "-i", container, "psql", "-U", "postgres", "-d", "billing", "-At", "-v", "ON_ERROR_STOP=1"}, Stdin: strings.NewReader(sql), Stdout: &out}); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(out.String())
	}
	query(b, "CREATE TABLE unwanted(id int); INSERT INTO invoices VALUES (3,999);")
	if query(a, "SELECT sum(amount) FROM invoices") != "30" {
		t.Fatal("restored stack altered original data")
	}
	if err = b.Probe(ctx, "changed", spec); err == nil {
		t.Fatal("application verification accepted changed data")
	}
	bad := snapshot
	bad.Source, err = store.PutArtifact("invalid", "application/vnd.envctl.postgres-dump", []byte("incomplete dump"))
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Restore(ctx, "failed_restore", spec, bad); err == nil {
		t.Fatal("invalid dump accepted")
	}
	// The failed restore has already rebuilt an empty target. Recreating the
	// adapter and retrying from retained bytes must repair that partial state.
	b = Postgres{Guest: b.Guest, Stack: b.Stack, Prepared: b.Prepared, Store: store, Revision: b.Revision}
	if err = b.Restore(ctx, "recovered_restore", spec, snapshot); err != nil {
		t.Fatal("restore recovery", err)
	}
	if query(b, "SELECT count(*) FROM pg_tables WHERE tablename='unwanted'") != "0" {
		t.Fatal("restore left objects absent from the snapshot")
	}
	if query(b, "SELECT sum(amount) FROM invoices") != "30" {
		t.Fatal("restored contents differ")
	}
	replay, err := a.Seed(ctx, "seed", spec, seed)
	if err != nil || replay.Source.Digest != snapshot.Source.Digest {
		t.Fatal("replay created a different dump", err)
	}
	if _, err = a.Capture(ctx, "capture", spec); err != nil {
		t.Fatal("capture", err)
	}
	t.Log("PostgreSQL seed, versioned capture, separate-stack restore, application verification, failed-restore recovery, exact reset and operation replay passed")
}
