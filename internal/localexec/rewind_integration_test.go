package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/tui"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type rewindAcceptance struct {
	Run               string `json:"run"`
	Original          string `json:"original"`
	Replacement       string `json:"replacement,omitempty"`
	Restored          string `json:"restored,omitempty"`
	GateInstalled     bool   `json:"gate_installed"`
	Overlap           bool   `json:"overlap"`
	Released          bool   `json:"released"`
	Restarted         bool   `json:"restarted"`
	RestoreVerified   bool   `json:"restore_verified"`
	OriginalDaemon    string `json:"original_daemon,omitempty"`
	ReplacementDaemon string `json:"replacement_daemon,omitempty"`
	RestoredDaemon    string `json:"restored_daemon,omitempty"`
}

// Allocates dedicated production VMs. Failures retain state and guest jobs for
// same-run resume; successful verification destroys only this fixture's VMs.
func TestRealActiveRewindAcrossDedicatedVMs(t *testing.T) {
	if os.Getenv("ENVCTL_REWIND_TEST") != "1" {
		t.Skip("opt-in real Codex and dedicated revision VMs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	dir := os.Getenv("ENVCTL_REWIND_RESUME")
	var proof rewindAcceptance
	var run *workflow.Run
	var err error
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "envctl-rewind-"+workflow.ID("acceptance"))
		run = rewindFixture(t, dir)
		proof = rewindAcceptance{Run: run.ID, Original: run.CurrentRevision}
	}
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	proofFile := filepath.Join(dir, "rewind-acceptance.json")
	if run != nil {
		if _, err = store.Create(ctx, "create", "real rewind acceptance", run); err != nil {
			t.Fatal(err)
		}
		if err = atomicJSON(proofFile, proof); err != nil {
			t.Fatal(err)
		}
	} else {
		raw, e := os.ReadFile(proofFile)
		if e != nil || json.Unmarshal(raw, &proof) != nil {
			t.Fatal("resume requires retained acceptance proof", e)
		}
		run, err = store.Get(ctx, proof.Run)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Log("retained rewind state:", dir)
	backend := New(store)
	server := httptest.NewServer((&daemon.Server{Store: store}).Handler())
	defer server.Close()
	first := &daemon.Client{HTTP: server.Client(), BaseURL: server.URL}
	second := &daemon.Client{HTTP: server.Client(), BaseURL: server.URL}
	var stop context.CancelFunc
	var done chan error
	start := func() {
		engineCtx, end := context.WithCancel(ctx)
		stop = end
		done = make(chan error, 1)
		e := &engine.Engine{Store: store, Backend: backend, Interval: 200 * time.Millisecond, OnError: func(err error) { t.Log("coordinator:", err) }}
		go func() { done <- e.Run(engineCtx) }()
	}
	start()
	defer func() { stop(); <-done }()
	save := func() {
		t.Helper()
		if err := atomicJSON(proofFile, proof); err != nil {
			t.Fatal(err)
		}
	}
	// Persist the exact API intent before sending it, allowing lost-response
	// replay without assigning another revision on a resumed test process.
	rewind := func(node, key string) (string, error) {
		file := filepath.Join(dir, key+".json")
		var req daemon.ActionRequest
		if raw, e := os.ReadFile(file); e == nil {
			if json.Unmarshal(raw, &req) != nil {
				return "", errors.New("invalid rewind request")
			}
		} else if os.IsNotExist(e) {
			req = daemon.ActionRequest{Action: "rewind", OperationID: workflow.ID("rewind"), Revision: run.CurrentRevision, ExpectedVersion: run.Version, Node: node}
			if e = atomicJSON(file, req); e != nil {
				return "", e
			}
		} else {
			return "", e
		}
		r, e := first.Action(ctx, run.ID, req)
		if errors.Is(e, workflow.ErrConflict) {
			return "", os.Remove(file)
		}
		if e != nil {
			return "", e
		}
		return r.CurrentRevision, nil
	}
	last := ""
	reported := time.Time{}
	for ctx.Err() == nil {
		run, err = first.Get(ctx, proof.Run)
		if err != nil {
			t.Fatal(err)
		}
		original := run.Revision(proof.Original)
		if original == nil {
			t.Fatal("original revision missing")
		}
		state := fmt.Sprintf("revision=%s state=%s checkpoints=%d attempts=%d old=%s", run.CurrentRevision, run.Current().State, len(run.Current().Checkpoints), len(run.Current().Attempts), original.State)
		if recovery := run.Current().Recovery; recovery != nil {
			state += " recovery=" + recovery.Detail
		}
		if state != last || time.Since(reported) > 20*time.Second {
			t.Log(state)
			last = state
			reported = time.Now()
		}
		for _, rev := range run.Revisions {
			if rev.State == "needs-attention" {
				t.Fatal("fixture requires recovery; retain and resume", rev.ID)
			}
		}
		if original.Runtime.Ready && !proof.GateInstalled {
			if err = backend.Provider.Exec(ctx, original.Runtime.ID, vm.Command{Args: []string{"sudo", "sh", "-c", "install -d -m 755 /work/envctl/revision-gate; touch /work/envctl/revision-gate/hold"}}); err != nil {
				t.Fatal(err)
			}
			proof.GateInstalled = true
			proof.OriginalDaemon = original.Runtime.DaemonID
			save()
		}
		oldCode := runningAttempt(original, "code")
		if proof.Replacement == "" && oldCode != nil && oldCode.Session != "" {
			a := engine.Assignment{Run: *run, Revision: *original, Attempt: *oldCode}
			status, e := backend.guest(a).Poll(ctx, oldCode.ID+"_worker", 0)
			if e != nil {
				t.Fatal(e)
			}
			if status.State == "running" {
				// This record exists only in the original worker's live database.
				rewindHTTP(t, ctx, backend, a, "PUT", "old-private", `{"owner":"original"}`)
				id, e := rewind("plan", "rewind-plan-request")
				if e != nil {
					t.Fatal(e)
				}
				if id != "" {
					proof.Replacement = id
					save()
					t.Log("rewound active Code to Plan through the client API")
				}
			}
		}
		if replacement := run.Revision(proof.Replacement); replacement != nil {
			planStarted := false
			for _, attempt := range replacement.Attempts {
				if attempt.Node == "plan" {
					planStarted = true
				}
			}
			if !proof.Overlap && replacement.Runtime.Ready && planStarted && len(replacement.ReadinessProblems(time.Now(), replacement.Requirements())) == 0 {
				if oldCode == nil {
					t.Fatal("old Code did not remain active during replacement Plan")
				}
				oldStatus, e := backend.guest(engine.Assignment{Revision: *original}).Poll(ctx, oldCode.ID+"_worker", 0)
				if e != nil || oldStatus.State != "running" {
					t.Fatal("old process not live during overlap", e)
				}
				if original.Runtime.ID == replacement.Runtime.ID || original.Runtime.DaemonID == replacement.Runtime.DaemonID {
					t.Fatal("overlap shares a VM or Docker daemon")
				}
				a := engine.Assignment{Run: *run, Revision: *replacement}
				if got := rewindHTTP(t, ctx, backend, a, "GET", "old-private", ""); !strings.Contains(got, "record missing") {
					t.Fatal("live old data leaked into replacement", got)
				}
				if got := rewindHTTP(t, ctx, backend, a, "GET", "tax-rate", ""); !strings.Contains(got, "0.2") {
					t.Fatal("replacement did not establish its verified baseline data", got)
				}
				for node, cp := range replacement.Checkpoints {
					if cp.Node != "task" && cp.ID == original.Checkpoints[node].ID {
						t.Fatal("replacement reused invalidated downstream checkpoint")
					}
				}
				verifyRewindClients(t, ctx, first, second, *run, proof.Original)
				proof.Overlap = true
				proof.ReplacementDaemon = replacement.Runtime.DaemonID
				save()
				t.Log("two live revision VMs, independent daemons/data, and two independent review clients verified")
			}
			if proof.Overlap && !proof.Released {
				if err = backend.Provider.Exec(ctx, original.Runtime.ID, vm.Command{Args: []string{"sudo", "touch", "/work/envctl/revision-gate/release"}}); err != nil {
					t.Fatal(err)
				}
				proof.Released = true
				save()
			}
			if proof.Overlap && !proof.Restarted {
				stop()
				if e := <-done; e != nil {
					t.Fatal(e)
				}
				backend = New(store)
				start()
				proof.Restarted = true
				save()
				t.Log("coordinator restarted with both revisions active")
			}
			if code, ok := replacement.Checkpoints["code"]; ok && proof.Restored == "" {
				if code.HistoricalOnly || code.Result.Commits["app"] == replacement.SourcePins["app"] {
					t.Fatal("replacement Code did not produce its own accepted source")
				}
				id, e := rewind("qa", "rewind-qa-request")
				if e != nil {
					t.Fatal(e)
				}
				if id != "" {
					proof.Restored = id
					save()
					t.Log("rewound QA to restore completed Code source, Compose input and dataset in another VM")
				}
			}
		}
		if restored := run.Revision(proof.Restored); restored != nil && restored.Runtime.Ready && !proof.RestoreVerified {
			// Successful probes precede admission and must use the restored Code
			// stack. Wait for their authoritative receipt before inspecting it.
			if len(restored.ReadinessProblems(time.Now(), restored.Requirements())) == 0 {
				replacement := run.Revision(proof.Replacement)
				code := replacement.Checkpoints["code"]
				if restored.FromCheckpoint != code.ID || restored.Checkpoints["code"].ID != code.ID {
					t.Fatal("wrong checkpoint restored")
				}
				if restored.Runtime.DaemonID == proof.ReplacementDaemon || restored.Runtime.DaemonID == proof.OriginalDaemon {
					t.Fatal("restoration reused an old daemon")
				}
				a := engine.Assignment{Run: *run, Revision: *restored}
				if got := rewindHTTP(t, ctx, backend, a, "GET", "phase", ""); !strings.Contains(got, "implemented") {
					t.Fatal("Code dataset was not restored", got)
				}
				if got := rewindHTTP(t, ctx, backend, a, "GET", "old-private", ""); !strings.Contains(got, "record missing") {
					t.Fatal("historical data contaminated selected checkpoint", got)
				}
				p, e := backend.currentStack(a)
				if e != nil {
					t.Fatal(e)
				}
				container, e := backend.stack(a).Container(ctx, p, "emulator")
				if e != nil {
					t.Fatal(e)
				}
				var out bytes.Buffer
				if e = backend.Provider.Exec(ctx, restored.Runtime.ID, vm.Command{Args: []string{"sudo", "docker", "exec", container, "printenv", "APP_REVISION"}, Stdout: &out}); e != nil || strings.TrimSpace(out.String()) != "implemented" {
					t.Fatal("restored Compose used original source", e)
				}
				baseline, e := backend.baselineData(ctx, a, p)
				if e != nil || workflow.Digest(baseline.Snapshots) != code.Result.DatasetDigest {
					t.Fatal("restored dataset manifest differs from Code", e)
				}
				proof.RestoreVerified = true
				proof.RestoredDaemon = restored.Runtime.DaemonID
				save()
				t.Log("selected Code source, changed Compose environment and exact dataset manifest verified in new VM")
			}
		}
		if proof.RestoreVerified && run.Current().State == "completed" && original.State == "superseded" && !original.Runtime.Ready {
			cp, ok := original.Checkpoints["code"]
			if !ok || !cp.HistoricalOnly {
				t.Fatal("old Code was not archived historically")
			}
			if _, ok := original.Checkpoints["qa"]; ok {
				t.Fatal("old Code continued to QA after rewind")
			}
			for _, a := range original.Attempts {
				if a.Node == "qa" {
					t.Fatal("old QA was dispatched")
				}
			}
			if !proof.Overlap || !proof.Restarted || len(run.Current().Checkpoints) != 5 {
				t.Fatal("incomplete rewind acceptance")
			}
			stop()
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			// Keep deferred shutdown balanced after stopping the scheduler.
			done = make(chan error, 1)
			done <- nil
			stop = func() {}
			for _, rev := range run.Revisions {
				if rev.Runtime.ID == "" {
					continue
				}
				if e := backend.Provider.Destroy(ctx, rev.Runtime.ID); e != nil && !errors.Is(e, vm.ErrMissing) {
					t.Fatal("fixture VM cleanup", e)
				}
			}
			if err = atomicJSON(filepath.Join(dir, "acceptance-cleaned.json"), map[string]any{"run": run.ID, "proof": proof, "at": time.Now().UTC()}); err != nil {
				t.Fatal(err)
			}
			t.Log("active Code drain, two-client review, coordinator restart, separate-VM source/Compose/data restore and terminal QA passed; owned VMs removed")
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(500 * time.Millisecond):
		}
	}
	t.Fatal("rewind acceptance observation timed out; resume the same retained run", ctx.Err())
}

func runningAttempt(rev *workflow.Revision, node string) *workflow.Attempt {
	for i := len(rev.Attempts) - 1; i >= 0; i-- {
		a := &rev.Attempts[i]
		if a.Node == node && a.State == "running" {
			return a
		}
	}
	return nil
}

func rewindHTTP(t *testing.T, ctx context.Context, b *Backend, a engine.Assignment, method, key, body string) string {
	t.Helper()
	var out bytes.Buffer
	script := `import sys,urllib.request,urllib.error
method,key,body=sys.argv[1:]
request=urllib.request.Request('http://127.0.0.1:18089/records/'+key,data=body.encode() if body else None,method=method,headers={'Content-Type':'application/json'})
try: response=urllib.request.urlopen(request,timeout=10)
except urllib.error.HTTPError as e: response=e
print(response.read().decode())`
	if err := b.Provider.Exec(ctx, a.Revision.Runtime.ID, vm.Command{Args: []string{"python3", "-c", script, method, key, body}, Stdout: &out}); err != nil {
		t.Fatal("guest fixture request failed", err)
	}
	return out.String()
}

func verifyRewindClients(t *testing.T, ctx context.Context, a, b *daemon.Client, run workflow.Run, old string) {
	t.Helper()
	ra, err := a.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	rb, err := b.Get(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	left, right := tui.New(a, "."), tui.New(b, ".")
	left.Runs = []workflow.Run{*ra}
	right.Runs = []workflow.Run{*rb}
	next, cmd := left.Update(tea.KeyPressMsg{Code: '[', Text: "["})
	left = next.(tui.Model)
	if cmd != nil || left.ViewedRevision != old || right.ViewedRevision != "" || !strings.Contains(left.View().Content, "historical view") {
		t.Fatal("independent client review selection failed")
	}
}

func rewindFixture(t *testing.T, dir string) *workflow.Run {
	t.Helper()
	source := filepath.Join(dir, "fixture")
	if err := os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"compose.yaml":       []byte("services:\n  emulator:\n    env_file: [app.env]\n    ports: ['127.0.0.1:18089:8080']\n"),
		"app.env":            []byte("APP_REVISION=base\n"),
		"calculator.py":      []byte("def add(a, b):\n    return a - b\n"),
		"test_calculator.py": []byte("import unittest\nfrom calculator import add\nclass Addition(unittest.TestCase):\n    def test_examples(self):\n        for a,b in [(2,3),(-2,1),(0,0)]: self.assertEqual(add(a,b),a+b)\n"),
	}
	for name, raw := range checkpoint.FixtureFiles("fixtures") {
		files["fixtures/"+name] = raw
	}
	for name, raw := range files {
		file := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, source, "init", "-q")
	fixtureGit(t, source, "add", ".")
	fixtureGit(t, source, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "rewind fixture baseline")
	c, err := workflow.Parse([]byte(fmt.Sprintf("version: 2\nproject: rewind\nrepositories: [{id: app, url: %q}]\nruntime: {memory_gib: 2}\nstack: {files: [compose.yaml, fixtures/compose.yaml]}\nworkflow: {template: feature}\n", source)))
	if err != nil {
		t.Fatal(err)
	}
	c.Dir = source
	c.Workflow.Template = ""
	delete(c.Workflow.Nodes, "approved-change")
	plan := c.Workflow.Nodes["plan"]
	plan.Requires = []string{"harness.worker", "harness.supervisor", "runtime.compose", "repositories.readwrite", "dataset.restore"}
	c.Workflow.Nodes["plan"] = plan
	code := c.Workflow.Nodes["code"]
	code.Prompt = `Before editing, if /work/envctl/revision-gate/hold exists, run a Python command that waits until /work/envctl/revision-gate/release exists (checking every second, at most 1200 seconds). This is a deliberate acceptance-test barrier: keep observing the running command until released; do not finish the stage while waiting. Do not modify those gate files. If hold is absent, proceed immediately. Then fix calculator.add, set app.env to APP_REVISION=implemented, and PUT {"value":"implemented"} as JSON to http://127.0.0.1:18089/records/phase. Preserve tests and the fixture service. The coordinator will rebuild Compose using app.env after your work. Return the implementation artifact.`
	c.Workflow.Nodes["code"] = code
	qa := c.Workflow.Nodes["qa"]
	qa.Checks = []workflow.Check{{Name: "arithmetic", Command: []string{"python3", "-m", "unittest", "discover", "-v"}}}
	c.Workflow.Nodes["qa"] = qa
	c.Data = workflow.DataConfig{Datasets: []workflow.Dataset{{ID: "emulator", Adapter: "http-fixture", Service: "emulator", SeedRepository: "app", SeedFile: "fixtures/seed.json", VerifyKey: "tax-rate", VerifyEquals: `{"rate":0.2}`}}}
	run, err := workflow.NewRun("dedicated VM rewind", "Fix calculator.add to return a+b, preserve its tests, set APP_REVISION=implemented in app.env and store the implemented phase in the emulator as instructed in Code. Complete QA locally. Planning/design stages only document the work; implementation and dataset changes belong to Code. No PR destination is part of this local review workflow.", "acceptance-test", c, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return run
}
