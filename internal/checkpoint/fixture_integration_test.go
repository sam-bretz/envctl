package checkpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
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

func TestRealHTTPFixtureCheckpointRestoreAndIsolation(t *testing.T) {
	if os.Getenv("ENVCTL_DATA_TEST") != "1" {
		t.Skip("opt-in real HTTP emulator guest stacks")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("select the owned acceptance VM explicitly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
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
	makeAdapter := func() Client {
		t.Helper()
		revision := workflow.ID("rev")
		root := "/work/envctl/fixtures/" + revision
		if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "mkdir", "-p", root}}); err != nil {
			t.Fatal(err)
		}
		for file, raw := range FixtureFiles(".") {
			if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "tee", root + "/" + file}, Stdin: bytes.NewReader(raw), Stdout: io.Discard}); err != nil {
				t.Fatal(err)
			}
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
			if err := provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "python3", "-c", "import shutil,sys; [shutil.rmtree(p,ignore_errors=True) for p in sys.argv[1:]]", root, "/work/envctl/datasets/" + revision}}); err != nil {
				t.Error(err)
			}
		})
		if err = stack.Up(ctx, prepared, 120); err != nil {
			t.Fatal(err)
		}
		return Client{Guest: guestjob.Client{Provider: provider, Runtime: name}, Stack: stack, Prepared: prepared, Store: store, Revision: revision}
	}
	a, b := makeAdapter(), makeAdapter()
	spec := workflow.Dataset{ID: "emulator", Adapter: "http-fixture", Service: "emulator", SeedRepository: "app", SeedFile: "seed.json", VerifyKey: "tax-rate", VerifyEquals: `{"rate":0.2}`}
	seed, err := store.PutArtifact("seed", spec.SeedMedia(), FixtureFiles(".")["seed.json"])
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := a.Seed(ctx, "seed", spec, seed)
	if err != nil {
		t.Fatal("seed", err)
	}
	if baseline.ToolVersion != "envctl-http-fixture/1.0.0" || baseline.Source.MediaType != spec.SnapshotMedia() || baseline.Evidence.Digest == "" {
		t.Fatal("missing typed snapshot evidence")
	}
	api := func(p Client, method, key, body string) (int, json.RawMessage) {
		t.Helper()
		container, err := p.Stack.Container(ctx, p.Prepared, spec.Service)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		code := `import json,sys,urllib.request,urllib.error
method,key,body=sys.argv[1:]
r=urllib.request.Request('http://127.0.0.1:8080/records/'+key,data=body.encode() if body else None,method=method,headers={'Content-Type':'application/json'})
try: response=urllib.request.urlopen(r,timeout=10)
except urllib.error.HTTPError as e: response=e
print(json.dumps({'status':response.status,'body':json.loads(response.read())}))`
		if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "docker", "exec", container, "python3", "-c", code, method, key, body}, Stdout: &out}); err != nil {
			t.Fatal(err)
		}
		var result struct {
			Status int
			Body   json.RawMessage
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result.Status, result.Body
	}
	if status, _ := api(a, "PUT", "invoice", `{"amount":42}`); status != 200 {
		t.Fatal("HTTP write failed")
	}
	snapshot, err := a.Capture(ctx, "capture", spec)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Source.Digest == baseline.Source.Digest {
		t.Fatal("capture ignored HTTP mutation")
	}
	if err := b.Restore(ctx, "restore", spec, snapshot); err != nil {
		t.Fatal(err)
	}
	if status, body := api(b, "GET", "invoice", ""); status != 200 || string(body) != `{"amount": 42}` {
		t.Fatalf("restored HTTP record: %d %s", status, body)
	}
	api(b, "PUT", "invoice", `{"amount":99}`)
	api(b, "PUT", "unwanted", `true`)
	if _, body := api(a, "GET", "invoice", ""); string(body) != `{"amount": 42}` {
		t.Fatal("separate stack mutation escaped its volume")
	}
	bad := snapshot
	bad.Source, err = store.PutArtifact("invalid", spec.SnapshotMedia(), []byte(`{"version":1,"records":{"tax-rate":{"rate":9}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Restore(ctx, "invalid_restore", spec, bad); err == nil {
		t.Fatal("invalid application snapshot accepted")
	}
	if _, body := api(b, "GET", "invoice", ""); string(body) != `{"amount": 99}` {
		t.Fatal("failed validation altered active data")
	}
	b = Client{Guest: b.Guest, Stack: b.Stack, Prepared: b.Prepared, Store: store, Revision: b.Revision}
	if err := b.Restore(ctx, "recovered_restore", spec, snapshot); err != nil {
		t.Fatal(err)
	}
	if status, _ := api(b, "GET", "unwanted", ""); status != 404 {
		t.Fatal("restore left a record absent from the snapshot")
	}
	if _, body := api(b, "GET", "invoice", ""); string(body) != `{"amount": 42}` {
		t.Fatal("restore failed to replace changed data")
	}
	if err := b.Probe(ctx, "probe", spec); err != nil {
		t.Fatal(err)
	}
	container, err := b.Stack.Container(ctx, b.Prepared, spec.Service)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "docker", "exec", container, "python3", "-c", "from pathlib import Path; Path('/data/state.json').write_text('interrupted or corrupt state')"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.Restore(ctx, "repair_corrupt_state", spec, snapshot); err != nil {
		t.Fatal("retained snapshot could not repair corrupt target data", err)
	}
	if _, body := api(b, "GET", "invoice", ""); string(body) != `{"amount": 42}` {
		t.Fatal("corrupt-target repair lost checkpoint contents")
	}
	api(a, "PUT", "invoice", `{"amount":77}`)
	replay, err := a.Capture(ctx, "capture", spec)
	if err != nil || replay.Source.Digest != snapshot.Source.Digest {
		t.Fatal("reconnection recaptured mutable data instead of replaying the operation", err)
	}
	fresh, err := a.Capture(ctx, "fresh_capture", spec)
	if err != nil || fresh.Source.Digest == snapshot.Source.Digest {
		t.Fatal("new operation did not capture current state", err)
	}
	wrong := snapshot
	wrong.Format = "postgres-custom-v1"
	if err := b.Restore(ctx, "wrong_format", spec, wrong); err == nil {
		t.Fatal("foreign snapshot format accepted")
	}
	t.Log("HTTP writes, seed/capture, isolated stack restoration, verification-before-replacement, exact reset, reconnect and operation replay passed")
}

func TestScaffoldFixtureRefusesOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fixture")
	if err := ScaffoldFixture(dir, "fixtures/http"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "seed.json"), []byte("user data"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ScaffoldFixture(dir, "fixtures/http"); err == nil {
		t.Fatal("existing fixture accepted")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "seed.json"))
	if err != nil || string(raw) != "user data" {
		t.Fatal("existing seed changed", err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "compose.yaml"))
	if err != nil || !strings.Contains(string(raw), `build: "fixtures/http"`) {
		t.Fatal("Compose context not rooted at workflow repository", err)
	}
}
