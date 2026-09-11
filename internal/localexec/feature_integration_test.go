package localexec

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/publication"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// This opt-in test has real external effects: fixture-only branches and draft
// PRs, each scoped to its random run ID. They are cleaned after verification.
// Failed runs retain their state and jobs for explicit same-run continuation.
func TestRealTwoRepositoryFeatureThroughPR(t *testing.T) {
	realFeature(t, false)
}

func TestRealFeatureWithBrowserDatasetsAndPlanAttachment(t *testing.T) {
	realFeature(t, true)
}

func realFeature(t *testing.T, extended bool) {
	if os.Getenv("ENVCTL_FEATURE_TEST") != "1" {
		t.Skip("requires real Codex, selected owned VM and GitHub fixture destination")
	}
	name, providerDir, destination := os.Getenv("ENVCTL_AGENT_VM"), os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_GITHUB_REPOSITORY")
	if name == "" || providerDir == "" || !regexp.MustCompile(`^[A-Za-z0-9_-]+/[A-Za-z0-9_.-]+$`).MatchString(destination) {
		t.Fatal("explicit owned VM, provider state and GitHub fixture destination required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	dir := os.Getenv("ENVCTL_FEATURE_RESUME")
	var run *workflow.Run
	var store *runstore.Store
	var err error
	if dir != "" {
		store, err = runstore.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		runs, e := store.List(ctx)
		if e != nil || len(runs) != 1 {
			t.Fatal("resume requires one retained fixture run", e)
		}
		run = &runs[0]
		if run.Current().Runtime.ID != name {
			t.Fatal("retained run belongs to another VM")
		}
	} else {
		cfg, err := workflow.Parse([]byte("version: 2\nproject: acceptance\nrepositories: [{id: api, url: /api}, {id: client, url: /client}]\nruntime: {memory_gib: 2}\nstack: {files: [compose.yaml]}\nworkflow:\n  template: feature\n  nodes:\n    qa:\n      checks:\n        - {name: api-unit, repository: api, command: [python3, -m, unittest, discover, -v]}\n        - {name: client-http, repository: client, command: [python3, -m, unittest, discover, -v]}\n"))
		if err != nil {
			t.Fatal(err)
		}
		fixtureID := workflow.ID("acceptance")
		dir = filepath.Join(os.TempDir(), "envctl-feature-"+fixtureID)
		if err = atomicJSON(filepath.Join(dir, "acceptance-setup.json"), map[string]string{"fixture": fixtureID, "destination": destination}); err != nil {
			t.Fatal(err)
		}
		repositories := []workflow.Repository{}
		for _, id := range []string{"api", "client"} {
			source := filepath.Join(dir, "fixtures", id)
			if err = os.MkdirAll(source, 0700); err != nil {
				t.Fatal(err)
			}
			files := featureFiles(id)
			if extended {
				files = extendedFeatureFiles(id)
			}
			for name, body := range files {
				if err = os.MkdirAll(filepath.Dir(filepath.Join(source, name)), 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(source, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			fixtureGit(t, source, "init", "-q", "-b", "main")
			fixtureGit(t, source, "add", ".")
			fixtureGit(t, source, "-c", "user.name=envctl acceptance", "-c", "user.email=envctl@localhost", "commit", "-qm", "Isolated envctl feature acceptance baseline")
			base := "envctl-acceptance/" + fixtureID + "/" + id + "/base"
			fixtureGit(t, source, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential", "push", "--porcelain", "--force-with-lease=refs/heads/"+base+":", "https://github.com/"+destination+".git", "HEAD:refs/heads/"+base)
			repositories = append(repositories, workflow.Repository{ID: id, URL: source, Ref: "HEAD", Publication: &workflow.PublicationTarget{Provider: "github", Repository: destination, Base: base}})
		}
		cfg.Repositories = repositories
		cfg.Dir = dir
		if extended {
			configureExtendedFeature(&cfg)
		}
		run, err = workflow.NewRun("envctl acceptance: addition API and client", "Implement addition in api/calculator.py and the HTTP client in client/client.py. Preserve all public signatures and fixed tests. add(a,b) returns the sum; the client posts two operands to the service and returns its numeric result. Verify positive, negative and zero inputs with the supplied unittest suites. The Compose service listens at http://127.0.0.1:18088 inside the guest. The coordinator rebuilds the API image after Code, before review and QA. Do not start a second Compose project or change the host port. Produce reviewed PRs for both repositories against their configured fixture bases.", "acceptance-test", cfg, time.Now().UTC())
		if err != nil {
			t.Fatal(err)
		}
		run.Current().State = "preparing"
		run.Current().Runtime = workflow.RuntimeState{ID: name, Provider: "lima", Location: "local", State: "preparing"}
		store, err = runstore.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = store.Create(ctx, "create", "real two-repository acceptance", run); err != nil {
			t.Fatal(err)
		}
	}
	defer func() { store.Close() }()
	t.Log("retained feature state:", dir)
	if extended != (len(run.Current().Config.Data.Datasets) > 0) {
		t.Fatal("resume fixture mode does not match the selected acceptance test")
	}
	backend := New(store)
	backend.Provider = vm.NewLima(providerDir)
	coordinator := &engine.Engine{Store: store, Backend: backend}
	last := ""
	restarted := false
	type restartReceipt struct {
		Run, Revision, Attempt, Session, ObservedState string
		At                                             time.Time
	}
	restartPath := filepath.Join(dir, "coordinator-restart.json")
	if raw, readErr := os.ReadFile(restartPath); readErr == nil {
		var proof restartReceipt
		if json.Unmarshal(raw, &proof) != nil || proof.Run != run.ID || proof.Revision != run.CurrentRevision || proof.ObservedState != "running" || proof.Session == "" || proof.At.IsZero() {
			t.Fatal("invalid coordinator restart evidence")
		}
		attempt := run.Current().Attempt(proof.Attempt)
		if attempt == nil || attempt.Node != "code" {
			t.Fatal("restart evidence refers to another stage")
		}
		restarted = true
	} else if !os.IsNotExist(readErr) {
		t.Fatal(readErr)
	}
	for ctx.Err() == nil {
		if extended {
			if advanceExtendedAttachment(t, ctx, coordinator, backend, run, name) {
				run, err = store.Get(ctx, run.ID)
				if err != nil {
					t.Fatal(err)
				}
				continue
			}
		}
		if err = coordinator.Reconcile(ctx, run.ID, run.CurrentRevision); err != nil {
			t.Fatal(err)
		}
		run, err = store.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		v := run.Current()
		summary := fmt.Sprintf("%s checkpoints=%d attempts=%d", v.State, len(v.Checkpoints), len(v.Attempts))
		if v.Recovery != nil {
			summary += " recovery=" + v.Recovery.Detail
		}
		if summary != last {
			t.Log(summary)
			last = summary
		}
		if v.State == "needs-attention" {
			t.Fatal("fixture requires recovery; retained state:", dir)
		}
		for _, attempt := range v.Attempts {
			if attempt.State == "awaiting-approval" {
				// Explicit fixture actor exercises the same user approval mutation;
				// this is not a claim that a human reviewed this synthetic feature.
				_, err = store.Mutate(ctx, run.ID, run.Version, workflow.ID("approval"), "acceptance.approved", attempt.ID, func(r *workflow.Run) error {
					return r.Approve(v.ID, attempt.ID, "acceptance-test", attempt.Result.WorkDigest(), time.Now())
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			if attempt.Node == "code" && attempt.State == "running" && attempt.Session != "" && !restarted {
				if err = store.Close(); err != nil {
					t.Fatal(err)
				}
				store, err = runstore.Open(dir)
				if err != nil {
					t.Fatal(err)
				}
				backend = New(store)
				backend.Provider = vm.NewLima(providerDir)
				coordinator = &engine.Engine{Store: store, Backend: backend}
				if err = atomicJSON(restartPath, restartReceipt{Run: run.ID, Revision: v.ID, Attempt: attempt.ID, Session: attempt.Session, ObservedState: attempt.State, At: time.Now().UTC()}); err != nil {
					t.Fatal(err)
				}
				restarted = true
				t.Log("coordinator recreated during real Code execution")
			}
		}
		if v.State == "completed" {
			if extended {
				verifyExtendedFeature(t, store, *run)
			}
			if len(v.Checkpoints) != 6 || len(v.Checkpoints["approved-change"].Result.PRs) != 2 || !restarted {
				t.Fatal("six-stage/multi-repository/restart evidence incomplete")
			}
			for id, cp := range v.Checkpoints {
				if !cp.Result.Review.Accepted || len(cp.Result.Sources) != 2 {
					t.Fatal("checkpoint lacks reviewed source evidence", id)
				}
			}
			for repo, link := range v.Checkpoints["approved-change"].Result.PRs {
				t.Log("verified PR", repo, link)
			}
			cleanupFeature(t, ctx, backend, *run, destination)
			t.Log("six real stages, two source repositories, independent QA, explicit fixture approval, exact GitHub PR heads, restart and cleanup verified")
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("feature acceptance timed out; resume its retained state:", dir)
}

func fixtureGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	raw, err := cmd.Output()
	if err != nil {
		t.Fatal("fixture Git operation failed", err)
	}
	return strings.TrimSpace(string(raw))
}
func cleanupFeature(t *testing.T, ctx context.Context, b *Backend, run workflow.Run, destination string) {
	t.Helper()
	v := run.Current()
	api := publication.CLI{}
	for _, repo := range v.Config.Repositories {
		if repo.Publication == nil || repo.Publication.Repository != destination || !strings.HasPrefix(repo.Publication.Base, "envctl-acceptance/") {
			t.Fatal("refusing cleanup outside acceptance namespace")
		}
		link := v.Checkpoints["approved-change"].Result.PRs[repo.ID]
		number := link[strings.LastIndex(link, "/")+1:]
		var pr struct {
			Body  string `json:"body"`
			State string `json:"state"`
		}
		endpoint := "repos/" + destination + "/pulls/" + number
		if err := api.Call(ctx, "GET", endpoint, nil, &pr); err != nil || !strings.Contains(pr.Body, "envctl:"+run.ID+":"+v.ID+":"+repo.ID+":") {
			t.Fatal("PR ownership check failed", err)
		}
		if err := api.Call(ctx, "PATCH", endpoint, map[string]string{"state": "closed"}, &pr); err != nil || pr.State != "closed" {
			t.Fatal("fixture PR cleanup failed", err)
		}
		for _, branch := range []string{workflow.OutputBranch(repo, v.ID), repo.Publication.Base} {
			if err := api.Call(ctx, "DELETE", "repos/"+destination+"/git/refs/heads/"+url.PathEscape(branch), nil, nil); err != nil {
				t.Fatal("fixture branch cleanup failed", err)
			}
		}
	}
	a := engine.Assignment{Run: run, Revision: *v}
	if err := b.cleanupPlugins(ctx, a); err != nil {
		t.Fatal("plugin cleanup", err)
	}
	if p, err := b.currentStack(a); err == nil {
		if err = b.stack(a).Down(ctx, p, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(b.Store.Dir, "acceptance-cleaned.json"), mustJSON(map[string]any{"run": run.ID, "revision": v.ID, "at": time.Now()}), 0600); err != nil {
		t.Fatal(err)
	}
}

func featureFiles(repo string) map[string]string {
	if repo == "client" {
		return map[string]string{
			"client.py":      "def remote_add(base_url, a, b):\n    raise NotImplementedError('implement the HTTP addition client')\n",
			"test_client.py": "import unittest\nfrom client import remote_add\nclass ClientTests(unittest.TestCase):\n    def test_http_addition(self):\n        for a, b in [(2,3),(-4,1),(0,0)]:\n            self.assertEqual(remote_add('http://127.0.0.1:18088',a,b), a+b)\n",
			".gitignore":     "__pycache__/\n.pytest_cache/\n",
		}
	}
	return map[string]string{
		"calculator.py":      "def add(a, b):\n    raise NotImplementedError('implement addition')\n",
		"test_calculator.py": "import unittest\nfrom calculator import add\nclass AdditionTests(unittest.TestCase):\n    def test_addition(self):\n        for a, b in [(2,3),(-4,1),(0,0)]:\n            self.assertEqual(add(a,b),a+b)\n",
		"server.py": `import json
from http.server import BaseHTTPRequestHandler, HTTPServer
from calculator import add
class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        self.send_response(200 if self.path == '/health' else 404)
        self.end_headers()
    def do_POST(self):
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        try:
            result = {'result': add(data['a'], data['b'])}
            self.send_response(200)
        except Exception:
            result = {'error': 'not implemented'}
            self.send_response(501)
        self.send_header('Content-Type', 'application/json')
        self.end_headers()
        self.wfile.write(json.dumps(result).encode())
HTTPServer(('0.0.0.0', 8080), Handler).serve_forever()
`,
		"Dockerfile": "FROM python:3.13-alpine\nWORKDIR /app\nCOPY calculator.py server.py ./\nCMD [\"python3\",\"server.py\"]\n",
		"compose.yaml": `services:
  api:
    build: .
    ports: ['18088:8080']
    healthcheck:
      test: [CMD, python3, -c, "import urllib.request; urllib.request.urlopen('http://127.0.0.1:8080/health')"]
      interval: 1s
      timeout: 2s
      retries: 20
`,
		".gitignore": "__pycache__/\n.pytest_cache/\n",
	}
}
