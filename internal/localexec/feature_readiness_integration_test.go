package localexec

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func configureExtendedFeature(c *workflow.Config) {
	c.Stack.Files = append(c.Stack.Files, "fixtures/compose.yaml")
	c.Data = workflow.DataConfig{Quiesce: []string{"api"}, Datasets: []workflow.Dataset{
		{ID: "billing", Adapter: "postgres", Service: "db", Database: "billing", User: "postgres", SeedRepository: "api", SeedFile: "billing.sql", VerifySQL: "SELECT sum(amount) FROM invoices", VerifyEquals: "30"},
		{ID: "emulator", Adapter: "http-fixture", Service: "emulator", SeedRepository: "api", SeedFile: "fixtures/seed.json", VerifyKey: "tax-rate", VerifyEquals: `{"rate":0.2}`},
	}}
	qa := c.Workflow.Nodes["qa"]
	qa.Requires = append(qa.Requires, "browser.test")
	qa.Checks = append(qa.Checks, workflow.Check{Name: "browser-addition", Plugin: "playwright", TimeoutSeconds: 120, Input: map[string]any{"steps": []any{
		map[string]any{"fill": map[string]any{"selector": "#left", "value": "2"}},
		map[string]any{"fill": map[string]any{"selector": "#right", "value": "3"}},
		map[string]any{"click": "#add"},
		map[string]any{"text": map[string]any{"selector": "#result", "equals": "5"}},
	}}})
	c.Workflow.Nodes["qa"] = qa
}

func extendedFeatureFiles(repo string) map[string]string {
	files := featureFiles(repo)
	if repo != "api" {
		return files
	}
	for name, raw := range checkpoint.FixtureFiles("fixtures") {
		files["fixtures/"+name] = string(raw)
	}
	files["billing.sql"] = "CREATE TABLE invoices(id integer primary key, amount integer); INSERT INTO invoices VALUES (1,10),(2,20);"
	files["compose.yaml"] += `  db:
    image: postgres@sha256:ef257d85f76e48da1c64832459b59fcaba1a4dac97bf5d7450c77753542eee94
    environment: {POSTGRES_HOST_AUTH_METHOD: trust}
    volumes: [billing-data:/var/lib/postgresql/data]
    healthcheck:
      test: [CMD, pg_isready, -U, postgres]
      interval: 1s
      timeout: 1s
      retries: 30
volumes: {billing-data: {}}
`
	files["Dockerfile"] = strings.ReplaceAll(files["Dockerfile"], "python:3.13-alpine", checkpoint.FixtureImage)
	files["server.py"] = strings.Replace(files["server.py"], "    def do_GET(self):\n", `    def do_GET(self):
        if self.path == '/':
            self.send_response(200)
            self.send_header('Content-Type', 'text/html')
            self.end_headers()
            self.wfile.write(b'''<!doctype html><title>Addition acceptance</title><h1>Addition</h1><input id="left"><input id="right"><button id="add">Add</button><output id="result"></output><script>document.querySelector('#add').onclick=async()=>{const r=await fetch('/',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({a:Number(document.querySelector('#left').value),b:Number(document.querySelector('#right').value)})});const v=await r.json();document.querySelector('#result').textContent=String(v.result)}</script>''')
            return
`, 1)
	return files
}

// This fixture reuses a single explicitly owned VM only after the old revision
// has drained and its stack is removed. Production scheduling still allocates
// dedicated VMs. This exercises Plan attachment, not overlapping VM isolation.
func advanceExtendedAttachment(t *testing.T, ctx context.Context, e *engine.Engine, b *Backend, run *workflow.Run, vmName string) bool {
	t.Helper()
	v := run.Current()
	mutate := func(kind string, fn func(*workflow.Run) error) {
		t.Helper()
		if _, err := b.Store.Mutate(ctx, run.ID, run.Version, workflow.ID("fixture"), kind, nil, fn); err != nil {
			t.Fatal(err)
		}
	}
	if v.State == "queued" {
		for _, old := range run.Revisions {
			if old.ID == v.ID || !old.Runtime.Ready {
				continue
			}
			if old.State == "draining" {
				if err := e.Reconcile(ctx, run.ID, old.ID); err != nil {
					t.Fatal("drain old Plan", err)
				}
				return true
			}
			if old.State != "superseded" {
				t.Fatal("fixture cannot reuse an active VM")
			}
			a := engine.Assignment{Run: *run, Revision: old}
			if err := b.cleanupPlugins(ctx, a); err != nil {
				t.Fatal(err)
			}
			if p, err := b.currentStack(a); err == nil {
				if err = b.stack(a).Down(ctx, p, true); err != nil {
					t.Fatal(err)
				}
			}
			mutate("fixture.revision-stack-released", func(r *workflow.Run) error {
				previous := r.Revision(old.ID)
				previous.Runtime.Ready = false
				previous.Runtime.State = "stopped"
				return nil
			})
			return true
		}
		mutate("fixture.vm-reused-after-drain", func(r *workflow.Run) error {
			r.Current().State = "preparing"
			r.Current().Runtime = workflow.RuntimeState{ID: vmName, Provider: "lima", Location: "local", State: "preparing"}
			return nil
		})
		return true
	}
	if len(v.Config.Plugins) > 0 && v.Config.Plugins[0].Config["base_url"] == "http://127.0.0.1:18088" {
		return false
	}
	for _, a := range v.Attempts {
		if a.Node != "task" && a.Node != "plan" {
			t.Fatal("downstream work started before browser readiness passed")
		}
	}
	if _, ok := v.Checkpoints["plan"]; ok {
		t.Fatal("missing/failed browser capability satisfied Plan")
	}
	var proposal *workflow.Attempt
	for i := range v.Attempts {
		a := &v.Attempts[i]
		if a.Node == "plan" && a.Result != nil {
			proposal = a
		}
	}
	if proposal == nil {
		return false
	}
	problems := strings.Join(v.ReadinessProblems(time.Now(), v.Requirements()), "\n")
	if !strings.Contains(problems, "browser.test") {
		return false
	}
	url := "http://127.0.0.1:18090"
	if len(v.Config.Plugins) > 0 {
		// Require an actual failed guest probe, not just an untested binding.
		found := false
		for _, p := range v.Readiness {
			if p.Capability == "browser.test" && !p.Passed && strings.HasPrefix(p.Binding, "playwright@") {
				found = true
			}
		}
		if !found {
			return false
		}
		url = "http://127.0.0.1:18088"
	}
	proof := map[string]any{"run": run.ID, "revision": v.ID, "attempt": proposal.ID, "problems": problems, "readiness": v.Readiness, "at": time.Now()}
	if err := atomicJSON(filepath.Join(b.Store.Dir, "attachment-proof-"+v.ID+".json"), proof); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer((&daemon.Server{Store: b.Store}).Handler())
	defer server.Close()
	client := daemon.Client{HTTP: server.Client(), BaseURL: server.URL}
	ref := workflow.PluginRef{ID: "playwright", Source: "builtin:playwright", Version: "1.0.0", Config: map[string]any{"base_url": url}}
	next, err := client.Action(ctx, run.ID, daemon.ActionRequest{OperationID: workflow.ID("attach"), ExpectedVersion: run.Version, Revision: v.ID, Action: "plugin-attach", Plugin: &ref})
	if err != nil {
		t.Fatal("invocation attachment", err)
	}
	if next.CurrentRevision == v.ID || len(next.Current().Config.Plugins) != 1 {
		t.Fatal("attachment did not create a frozen revision")
	}
	t.Log("real Plan held for missing/failed browser readiness; attached invocation binding", url)
	return true
}

func verifyExtendedFeature(t *testing.T, store *runstore.Store, run workflow.Run) {
	t.Helper()
	v := run.Current()
	if len(run.Revisions) != 3 {
		t.Fatal("missing capability and failed-probe amendments not demonstrated")
	}
	for _, old := range run.Revisions[:len(run.Revisions)-1] {
		if !old.Checkpoints["plan"].HistoricalOnly || len(old.Checkpoints) > 2 {
			t.Fatal("superseded Plan did not stay historical")
		}
	}
	for id, cp := range v.Checkpoints {
		kind := v.Config.Workflow.Nodes[id].Kind
		if kind != "task" && kind != "plan" && (len(cp.Result.Datasets) != 2 || cp.Result.DatasetDigest != workflow.Digest(cp.Result.Datasets)) {
			t.Fatal("missing reviewed PostgreSQL/emulator checkpoint", id)
		}
	}
	found := false
	qa := v.Checkpoints["qa"].Result
	for _, check := range qa.Checks {
		if check.Name != "browser-addition" {
			continue
		}
		if !check.Passed || check.CommitsDigest != workflow.Digest(qa.Commits) {
			t.Fatal("browser check not bound to QA commits")
		}
		raw, err := store.Artifact(check.EvidenceDigest)
		if err != nil {
			t.Fatal(err)
		}
		var response map[string]any
		if json.Unmarshal(raw, &response) != nil || !strings.Contains(string(raw), "screenshot_png") {
			t.Fatal("browser screenshot evidence missing")
		}
		found = true
	}
	if !found {
		t.Fatal("real browser check did not execute")
	}
	if v.Checkpoints["approved-change"].Result.DatasetDigest != qa.DatasetDigest {
		t.Fatal("publication changed the accepted QA dataset")
	}
	t.Log("missing plugin -> failed real probe -> repaired invocation -> accepted Plan -> browser QA with both dataset manifests verified")
}
