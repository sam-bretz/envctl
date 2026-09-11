package localexec

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Real provider/backend component acceptance. Workers' model reasoning and the
// full scheduler fan-out/join demonstration remain separate acceptance checks.
func TestRealChildVMSourceAndDatasetIsolation(t *testing.T) {
	if os.Getenv("ENVCTL_CHILD_VM_TEST") != "1" {
		t.Skip("opt-in two new child VM acceptance")
	}
	b, a := fixture(t)
	dir := filepath.Join(os.TempDir(), "envctl-child-"+workflow.ID("acceptance"))
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b.Store = store
	b.Provider = vm.NewLima(dir)
	t.Log("retained child acceptance state:", dir)
	ctx, cancel := context.WithTimeout(context.Background(), 18*time.Minute)
	defer cancel()
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
	a.Revision.Config.Repositories = []workflow.Repository{{ID: "app", URL: root, Ref: "HEAD"}}
	a.Revision.Config.Runtime.MemoryGiB = 2
	a.Revision.Config.Limits.Parallel, a.Revision.Config.Limits.VMs = 2, 3
	a.Revision.Config.Stack.Files = []string{"compose.yaml"}
	a.Revision.Config.Data = workflow.DataConfig{Datasets: []workflow.Dataset{{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "app", SeedFile: "seed.sql", VerifySQL: "SELECT count(*) > 0 FROM invoices", VerifyEquals: "t"}}}
	resolved, _, err := (repository.Cache{Dir: filepath.Join(dir, "source-cache")}).Prepare(ctx, repository.Resolver{Dir: filepath.Join(dir, "baseline")}, a.Revision.Config.Repositories[0])
	if err != nil {
		t.Fatal(err)
	}
	a.Revision.SourcePins = map[string]string{"app": resolved.Repository.BaseSHA}
	a.Revision.Checkpoints = map[string]workflow.Checkpoint{}
	a.Revision.ChildRuntimes = map[string]*workflow.ChildRuntime{}
	for _, node := range []string{"left", "right"} {
		a.Revision.ChildRuntimes[node] = &workflow.ChildRuntime{Runtime: workflow.RuntimeState{ID: "envctl-" + strings.ReplaceAll(a.Revision.ID, "_", "-") + "-" + node, Provider: "lima", Location: "local", State: "preparing"}}
		a.Revision.Config.Workflow.Nodes[node] = workflow.Node{Kind: "custom", Outputs: []string{"result"}, Writes: []string{"app"}}
	}
	assignments := make([]engine.Assignment, 2)
	for i, node := range []string{"left", "right"} {
		assignments[i] = workflow.Clone(a)
		assignments[i].Child = node
		assignments[i].Revision.Runtime = assignments[i].Revision.ChildRuntimes[node].Runtime
		assignments[i].Attempt = workflow.Attempt{ID: workflow.ID("attempt"), Node: node, State: "running", Inputs: map[string]string{}}
	}
	if err := atomicJSON(filepath.Join(dir, "acceptance-inputs.json"), assignments); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		for _, assignment := range assignments {
			if err := b.Release(clean, assignment); err != nil {
				t.Error("owned child VM cleanup:", err)
			}
		}
	})
	// The source origin is gone before either child is provisioned. Both use
	// the exact retained baseline, with no shared mount or host Git dependency.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := range assignments {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			prepared, err := b.Prepare(ctx, assignments[i])
			if err != nil {
				errs[i] = err
				return
			}
			assignments[i].Revision.Runtime = prepared.Runtime
			assignments[i].Revision.ChildRuntimes[assignments[i].Child].Runtime = prepared.Runtime
			errs[i] = b.Start(ctx, assignments[i]) // freezes a worker request; no model job is submitted
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("child %d preparation: %v", i, err)
		}
	}
	left, right := assignments[0], assignments[1]
	if left.Revision.Runtime.ID == right.Revision.Runtime.ID || left.Revision.Runtime.DaemonID == right.Revision.Runtime.DaemonID {
		t.Fatal("children share VM or Docker identity")
	}
	leftDir, _ := b.dir(left)
	rightDir, _ := b.dir(right)
	if leftDir == rightDir {
		t.Fatal("host execution receipts share an owner")
	}
	query := func(assignment engine.Assignment, sql string) string {
		t.Helper()
		p, err := b.currentStack(assignment)
		if err != nil {
			t.Fatal(err)
		}
		container, err := b.stack(assignment).Container(ctx, p, "db")
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		if err := b.Provider.Exec(ctx, assignment.Revision.Runtime.ID, vm.Command{Args: []string{"sudo", "docker", "exec", "-i", container, "psql", "-U", "postgres", "-d", "billing", "-At", "-v", "ON_ERROR_STOP=1"}, Stdin: strings.NewReader(sql), Stdout: &output}); err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(output.String())
	}
	if query(left, "SELECT sum(amount) FROM invoices") != "30" || query(right, "SELECT sum(amount) FROM invoices") != "30" {
		t.Fatal("children did not restore the same pinned seed")
	}
	query(left, "UPDATE invoices SET amount=111")
	if query(right, "SELECT sum(amount) FROM invoices") != "30" {
		t.Fatal("left mutation leaked into right database")
	}
	lrecord, err := b.load(left)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Provider.Exec(ctx, left.Revision.Runtime.ID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "python3", "-c", "import pathlib,sys;(pathlib.Path(sys.argv[1])/'work.txt').write_text('left private edit\\n')", lrecord.Directories["app"]}}); err != nil {
		t.Fatal(err)
	}
	rrecord, err := b.load(right)
	if err != nil {
		t.Fatal(err)
	}
	var rightSource bytes.Buffer
	if err := b.Provider.Exec(ctx, right.Revision.Runtime.ID, vm.Command{Args: []string{"sudo", "cat", rrecord.Directories["app"] + "/work.txt"}, Stdout: &rightSource}); err != nil || rightSource.String() != "baseline\n" {
		t.Fatal("source mutation leaked across children", err)
	}
	if err := b.beginData(ctx, left, *lrecord.Stack, "fixture_recovery"); err != nil {
		t.Fatal(err)
	}
	if !b.dataBusy(left) || b.dataBusy(right) {
		t.Fatal("quiescence ownership leaked to sibling")
	}
	if err := b.captureData(ctx, right, rrecord); err != nil {
		t.Fatal("left recovery prevented right checkpoint:", err)
	}
	// Backend recreation reconnects each branch's own durable requests/data.
	b = &Backend{Store: store, Provider: vm.NewLima(dir)}
	if err := b.Start(ctx, right); err != nil {
		t.Fatal("right replay", err)
	}
	if query(left, "SELECT sum(amount) FROM invoices") != "222" || query(right, "SELECT sum(amount) FROM invoices") != "30" {
		t.Fatal("replay mixed branch data")
	}
	if err := b.endData(ctx, left, *lrecord.Stack); err != nil {
		t.Fatal(err)
	}
	if err := b.Release(ctx, left); err != nil {
		t.Fatal(err)
	}
	if query(right, "SELECT sum(amount) FROM invoices") != "30" {
		t.Fatal("releasing left stopped or changed right")
	}
	if err := b.Release(ctx, right); err != nil {
		t.Fatal(err)
	}
	proof := map[string]any{"left": left.Revision.Runtime, "right": right.Revision.Runtime, "separate_source": true, "separate_data": true, "independent_quiescence": true, "replay": true, "released": true}
	if err := atomicJSON(filepath.Join(dir, "acceptance-proof.json"), proof); err != nil {
		t.Fatal(err)
	}
	t.Log(fmt.Sprintf("independent child source/Compose/data and replay verified; both VMs released; proof %s", dir))
}
