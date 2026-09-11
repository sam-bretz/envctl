package localexec

import (
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

	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type parallelAcceptance struct {
	Run           string                           `json:"run"`
	Overlap       bool                             `json:"overlap"`
	Restarted     bool                             `json:"restarted"`
	OriginRemoved bool                             `json:"origin_removed"`
	LeftReleased  bool                             `json:"left_released"`
	FailedLeft    string                           `json:"failed_left,omitempty"`
	RightAttempt  string                           `json:"right_attempt,omitempty"`
	RetryReleased bool                             `json:"retry_released"`
	RightReleased bool                             `json:"right_released"`
	RetryIsolated bool                             `json:"retry_isolated"`
	Verified      bool                             `json:"verified"`
	Cancelling    bool                             `json:"cancelling"`
	Runtimes      map[string]workflow.RuntimeState `json:"runtimes"`
}

// Runs the production scheduler and real worker/supervisor pairs (Codex unless
// ENVCTL_PARALLEL_HARNESS and ENVCTL_PARALLEL_CREDENTIAL select another). Failed
// or interrupted observations retain the same run, VMs and guest jobs for an
// explicit ENVCTL_PARALLEL_RESUME; only successful acceptance tears them down.
func TestRealParallelAgentsRetryMergeAndCoordinatorRestart(t *testing.T) {
	if os.Getenv("ENVCTL_PARALLEL_TEST") != "1" {
		t.Skip("opt-in real parallel agents and dedicated VMs")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	dir := os.Getenv("ENVCTL_PARALLEL_RESUME")
	var run *workflow.Run
	var proof parallelAcceptance
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "envctl-parallel-"+workflow.ID("acceptance"))
		run = parallelFixture(t, dir)
		proof = parallelAcceptance{Run: run.ID, Runtimes: map[string]workflow.RuntimeState{}}
	}
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	proofFile := filepath.Join(dir, "parallel-acceptance.json")
	save := func() {
		t.Helper()
		if err := atomicJSON(proofFile, proof); err != nil {
			t.Fatal(err)
		}
	}
	if run != nil {
		if _, err := store.Create(ctx, "create", "parallel agent acceptance", run); err != nil {
			t.Fatal(err)
		}
		save()
	} else {
		raw, err := os.ReadFile(proofFile)
		if err != nil || json.Unmarshal(raw, &proof) != nil {
			t.Fatal("resume requires retained acceptance state", err)
		}
		run, err = store.Get(ctx, proof.Run)
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Log("retained parallel acceptance state:", dir)
	backend := New(store)
	server := httptest.NewServer((&daemon.Server{Store: store}).Handler())
	defer server.Close()
	client := &daemon.Client{HTTP: server.Client(), BaseURL: server.URL}
	var stop context.CancelFunc
	var done chan error
	start := func() {
		engineCtx, end := context.WithCancel(ctx)
		stop = end
		done = make(chan error, 1)
		coordinator := &engine.Engine{Store: store, Backend: backend, Interval: 200 * time.Millisecond, OnError: func(err error) { t.Log("coordinator:", err) }}
		go func() { done <- coordinator.Run(engineCtx) }()
	}
	stopEngine := func() error {
		if done == nil {
			return nil
		}
		stop()
		pending := done
		done = nil
		return <-pending
	}
	start()
	defer func() {
		if err := stopEngine(); err != nil {
			t.Error(err)
		}
	}()
	assignment := func(v *workflow.Revision, attempt *workflow.Attempt) engine.Assignment {
		a := engine.Assignment{Run: workflow.Clone(*run), Revision: workflow.Clone(*v), Attempt: workflow.Clone(*attempt), Child: attempt.Node}
		child := v.ChildRuntimes[attempt.Node]
		if child == nil {
			t.Fatal("missing child runtime", attempt.Node)
		}
		a.Revision.Runtime = workflow.Clone(child.Runtime)
		a.Revision.Readiness = workflow.Clone(child.Readiness)
		return a
	}
	gate := func(a engine.Assignment, name string) {
		t.Helper()
		if err := backend.Provider.Exec(ctx, a.Revision.Runtime.ID, vm.Command{Args: []string{"sudo", "sh", "-c", "install -d -m 755 /work/envctl/parallel-gate; touch \"/work/envctl/parallel-gate/$1\"", "envctl-test-gate", name}}); err != nil {
			t.Fatal(err)
		}
	}
	live := func(v *workflow.Revision, node string) (*workflow.Attempt, bool) {
		a := runningAttempt(v, node)
		if a == nil || a.Session == "" {
			return a, false
		}
		binding := assignment(v, a)
		status, err := backend.guest(binding).Poll(ctx, a.ID+"_worker", 0)
		if err != nil {
			t.Fatal("observe the same guest job", err)
		}
		return a, status.State == "running" || status.State == "starting"
	}
	last := ""
	reported := time.Time{}
	for ctx.Err() == nil {
		run, err = client.Get(ctx, proof.Run)
		if err != nil {
			t.Fatal(err)
		}
		v := run.Current()
		if run.VMCount() > v.Config.Limits.VMs {
			t.Fatal("production scheduler exceeded VM capacity")
		}
		state := fmt.Sprintf("state=%s checkpoints=%d attempts=%d owned_vms=%d", v.State, len(v.Checkpoints), len(v.Attempts), run.VMCount())
		if v.Recovery != nil {
			state += " recovery=" + v.Recovery.Detail
		}
		for _, node := range []string{"left", "right", "merge", "qa"} {
			if child := v.ChildRuntimes[node]; child != nil {
				state += " " + node + "=" + child.Runtime.State
				if child.Recovery != nil {
					state += " recovery=" + child.Recovery.Detail
				}
				if child.Runtime.Ready && child.Runtime.DaemonID != "" {
					proof.Runtimes[node] = child.Runtime
				}
			}
		}
		if v.Runtime.Ready {
			proof.Runtimes["parent"] = v.Runtime
		}
		if state != last || time.Since(reported) > 20*time.Second {
			t.Log(state)
			last = state
			reported = time.Now()
			save()
		}
		if v.State == "needs-attention" {
			t.Fatal("parallel fixture needs correction; retain this run and resume")
		}
		if _, ok := v.Checkpoints["plan"]; ok && !proof.OriginRemoved {
			if err := os.RemoveAll(filepath.Join(dir, "fixture")); err != nil {
				t.Fatal(err)
			}
			proof.OriginRemoved = true
			save()
			t.Log("removed original host source after Plan; downstream VMs must restore pinned retained inputs")
		}
		if !proof.Overlap {
			left, leftLive := live(v, "left")
			right, rightLive := live(v, "right")
			if leftLive && rightLive {
				l, r := v.ChildRuntimes["left"].Runtime, v.ChildRuntimes["right"].Runtime
				if l.ID == r.ID || l.DaemonID == r.DaemonID || l.DaemonID == v.Runtime.DaemonID || r.DaemonID == v.Runtime.DaemonID {
					t.Fatal("parallel agents share runtime ownership")
				}
				if workflow.Digest(left.Inputs) != workflow.Digest(right.Inputs) {
					t.Fatal("fan-out did not bind the same Plan checkpoint")
				}
				proof.Overlap = true
				proof.RightAttempt = right.ID
				save()
				t.Log("both real worker jobs are live in distinct child VMs")
			}
		}
		if proof.Overlap && !proof.Restarted {
			if err := stopEngine(); err != nil {
				t.Fatal(err)
			}
			// Engine shutdown drops transport only. The new engine reconnects to
			// these same guest job IDs; this is not a fresh invocation or worker.
			backend = New(store)
			start()
			proof.Restarted = true
			save()
			t.Log("recreated coordinator/backend while both real guest workers remain active")
		}
		if proof.Restarted && !proof.LeftReleased {
			a := runningAttempt(v, "left")
			if a == nil {
				t.Fatal("left ended before its barrier was released")
			}
			gate(assignment(v, a), "release")
			proof.LeftReleased = true
			save()
		}
		if proof.LeftReleased && proof.FailedLeft == "" {
			for _, a := range v.Attempts {
				if a.Node == "left" && a.State == "failed" {
					if !strings.Contains(a.Error, "retry-gate") {
						t.Fatal("left failed outside the intended retry gate; retain evidence", a.Error)
					}
					proof.FailedLeft = a.ID
					save()
					t.Log("left's real worker reached the deliberate failing check; retry is required")
				}
			}
		}
		if proof.FailedLeft != "" && !proof.RetryReleased {
			right, ok := live(v, "right")
			if !ok || right.ID != proof.RightAttempt {
				t.Fatal("right worker did not survive left failure")
			}
			body := rewindHTTP(t, ctx, backend, assignment(v, right), "GET", "branch", "")
			if strings.Contains(body, "left") {
				t.Fatal("left's mutable fixture leaked into right")
			}
			prior := v.Attempt(proof.FailedLeft)
			if prior == nil {
				t.Fatal("failed attempt evidence missing")
			}
			gate(assignment(v, prior), "retry-ready")
			proof.RetryReleased = true
			save()
		}
		if _, ok := v.Checkpoints["left"]; ok && !proof.RightReleased {
			right, liveRight := live(v, "right")
			if !liveRight || right.ID != proof.RightAttempt {
				t.Fatal("left retry restarted or stopped its sibling")
			}
			count := 0
			for _, a := range v.Attempts {
				if a.Node == "left" {
					count++
				}
			}
			if count < 2 || !proof.RetryReleased {
				t.Fatal("independent retry was not observed")
			}
			proof.RetryIsolated = true
			save()
			gate(assignment(v, right), "release")
			proof.RightReleased = true
			save()
			t.Log("left retry checkpointed while the original right worker stayed live; released right")
		}
		if v.State == "completed" && !proof.Verified {
			verifyParallelResult(t, ctx, store, run, proof)
			proof.Verified = true
			save()
			t.Log("real fan-out, independent retry, supervisor reviews, verified merge, dataset selection and QA passed")
		}
		if proof.Verified && !proof.Cancelling {
			if v.State != "cancelled" {
				_, err = client.Action(ctx, run.ID, daemon.ActionRequest{Action: "cancel", OperationID: "parallel-acceptance-cleanup", Revision: v.ID, ExpectedVersion: run.Version})
				if errors.Is(err, workflow.ErrConflict) {
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			proof.Cancelling = true
			save()
		}
		if proof.Cancelling && run.VMCount() == 0 {
			for _, runtime := range proof.Runtimes {
				if _, err := backend.Provider.Inspect(ctx, runtime.ID); !errors.Is(err, vm.ErrMissing) {
					t.Fatal("owned runtime cleanup not confirmed", runtime.ID, err)
				}
			}
			if err := atomicJSON(filepath.Join(dir, "acceptance-cleaned.json"), proof); err != nil {
				t.Fatal(err)
			}
			t.Log("all fixture parent/child VMs removed; complete evidence retained:", dir)
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(400 * time.Millisecond):
		}
	}
	t.Fatal("parallel acceptance observation ended; keep run/VMs and resume the same fixture:", ctx.Err())
}

func verifyParallelResult(t *testing.T, ctx context.Context, store *runstore.Store, run *workflow.Run, proof parallelAcceptance) {
	t.Helper()
	v := run.Current()
	if !proof.Overlap || !proof.Restarted || !proof.RetryIsolated || !proof.OriginRemoved || len(v.Checkpoints) != 6 {
		t.Fatal("incomplete parallel acceptance")
	}
	for _, node := range []string{"task", "plan", "left", "right", "merge", "qa"} {
		cp, ok := v.Checkpoints[node]
		if !ok || cp.HistoricalOnly || !cp.Result.Review.Accepted || cp.Result.Review.ResultDigest != cp.Result.WorkDigest() {
			t.Fatal("checkpoint lacks exact supervisor review", node)
		}
		if _, err := store.Artifact(cp.Result.Review.EvidenceDigest); err != nil {
			t.Fatal(err)
		}
		for _, a := range cp.Result.Sources {
			if _, err := store.Artifact(a.Digest); err != nil {
				t.Fatal(err)
			}
		}
		for _, a := range cp.Result.SourceObjects {
			if _, err := store.Artifact(a.Digest); err != nil {
				t.Fatal(err)
			}
		}
		for _, check := range cp.Result.Checks {
			if !check.Passed || check.CommitsDigest != workflow.Digest(cp.Result.Commits) {
				t.Fatal("unbound check", node)
			}
			if _, err := store.Artifact(check.EvidenceDigest); err != nil {
				t.Fatal(err)
			}
		}
		if node == "task" || node == "plan" {
			continue
		}
		raw, err := store.Artifact(cp.Result.Datasets["emulator"].Source.Digest)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot struct {
			Records map[string]json.RawMessage `json:"records"`
		}
		if json.Unmarshal(raw, &snapshot) != nil {
			t.Fatal("invalid retained fixture snapshot")
		}
		var branch struct {
			Owner string `json:"owner"`
		}
		if json.Unmarshal(snapshot.Records["branch"], &branch) != nil {
			t.Fatal("checkpoint lacks branch dataset", node)
		}
		want := "left"
		if node == "right" {
			want = "right"
		}
		if branch.Owner != want {
			t.Fatal("dataset join/isolation differs", node, branch.Owner)
		}
	}
	left, right, merge, qa := v.Checkpoints["left"], v.Checkpoints["right"], v.Checkpoints["merge"], v.Checkpoints["qa"]
	if left.Result.Commits["app"] == right.Result.Commits["app"] || merge.Inputs["left"] != left.ID || merge.Inputs["right"] != right.ID || qa.Inputs["merge"] != merge.ID {
		t.Fatal("join lost divergent source/input checkpoint identities")
	}
	if workflow.Digest(merge.Result.Commits) != workflow.Digest(qa.Result.Commits) {
		t.Fatal("QA changed merged source")
	}
	raw, err := store.Artifact(merge.Result.Sources["app"].Digest)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.VerifyMerge(ctx, raw, merge.Result.Commits["app"], []string{left.Result.Commits["app"], right.Result.Commits["app"]}); err != nil {
		t.Fatal(err)
	}
	// Inspect retained source without executing any worker-produced host code.
	dir := t.TempDir()
	bundle := filepath.Join(dir, "source.bundle")
	if err := os.WriteFile(bundle, raw, 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, dir, "-c", "core.hooksPath=/dev/null", "init", "--bare", "--template=")
	fixtureGit(t, dir, "-c", "core.hooksPath=/dev/null", "fetch", "--no-recurse-submodules", "--no-tags", bundle, merge.Result.Commits["app"])
	if got := fixtureGit(t, dir, "show", merge.Result.Commits["app"]+":verify.py"); strings.TrimSpace(got) != strings.TrimSpace(parallelVerify) {
		t.Fatal("worker changed immutable acceptance checks")
	}
	attempts := 0
	for _, a := range v.Attempts {
		if a.Node == "right" {
			attempts++
		}
	}
	if attempts != 1 {
		t.Fatal("independent right worker was retried", attempts)
	}
}

const parallelVerify = `import json,pathlib,sys,urllib.request
from calculator import add,subtract,FEATURES
mode=sys.argv[1]
if mode=='left':
    assert pathlib.Path('/work/envctl/parallel-gate/retry-ready').exists(), 'retry-gate: deliberately closed once by the acceptance coordinator; do not change this test'
if mode in ('left','all','qa'):
    assert [add(2,3),add(-2,1),add(0,0)]==[5,-1,0]
if mode in ('right','all','qa'):
    assert [subtract(7,2),subtract(-2,1),subtract(0,0)]==[5,-3,0]
expected={'left':{'add'},'right':{'subtract'},'all':{'add','subtract'},'qa':{'add','subtract'}}[mode]
assert set(FEATURES)==expected, (FEATURES,expected)
with urllib.request.urlopen('http://127.0.0.1:18089/records/branch',timeout=10) as response:
    branch=json.load(response)
assert branch=={'owner':'right' if mode=='right' else 'left'}, branch
print('verified',mode)
`

func parallelFixture(t *testing.T, dir string) *workflow.Run {
	t.Helper()
	root := filepath.Join(dir, "fixture")
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{"compose.yaml": []byte("services:\n  emulator:\n    ports: ['127.0.0.1:18089:8080']\n"), "calculator.py": []byte("FEATURES = ()\n\ndef add(a,b):\n    return a-b\n\ndef subtract(a,b):\n    return a+b\n"), "verify.py": []byte(parallelVerify), ".gitignore": []byte("__pycache__/\n*.pyc\n")}
	for name, body := range checkpoint.FixtureFiles("fixtures") {
		files["fixtures/"+name] = body
	}
	for name, body := range files {
		file := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(file), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
	fixtureGit(t, root, "init", "-q")
	fixtureGit(t, root, "add", ".")
	fixtureGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "parallel baseline")
	agents := ""
	if kind := os.Getenv("ENVCTL_PARALLEL_HARNESS"); kind != "" {
		h := fmt.Sprintf("{kind: %s, credential: %q}", kind, os.Getenv("ENVCTL_PARALLEL_CREDENTIAL"))
		agents = fmt.Sprintf("agents: {worker: %s, supervisor: %s}\n", h, h)
	}
	c, err := workflow.Parse([]byte(fmt.Sprintf("version: 2\nproject: parallel\nrepositories: [{id: app, url: %q}]\nruntime: {memory_gib: 2}\nlimits: {parallel: 2, vms: 3, max_attempts: 6, attempt_seconds: 1800}\nstack: {files: [compose.yaml, fixtures/compose.yaml]}\nworkflow: {template: feature}\n%s", root, agents)))
	if err != nil {
		t.Fatal(err)
	}
	c.Dir = root
	c.Workflow.Template = ""
	task, plan := c.Workflow.Nodes["task"], c.Workflow.Nodes["plan"]
	plan.Requires = []string{"harness.worker", "harness.supervisor", "runtime.compose", "repositories.readwrite", "dataset.restore", "workflow.checks"}
	plan.Prompt = "Document the branch ownership, fixed checks, selected left dataset at the join, and coordinator-owned acceptance barriers. No external publication, browser or additional service is required."
	barrier := "Before editing or proposing output, run a Python command that waits for /work/envctl/parallel-gate/release (check every second, at most 1500 seconds). Keep observing the command until released. This is a deliberate coordinator-owned acceptance barrier, not a missing external capability. Do not create or modify files in that gate directory. On a resumed attempt the release may already exist; continue immediately. "
	c.Workflow.Nodes = map[string]workflow.Node{
		"task": task, "plan": plan,
		"left":  {Kind: "code", Needs: []string{"plan"}, Writes: []string{"app"}, Outputs: []string{"implementation"}, Checks: []workflow.Check{{Name: "left-acceptance", Command: []string{"python3", "verify.py", "left"}}}, Prompt: barrier + "Fix only add(a,b) in calculator.py, set FEATURES=('add',), and PUT JSON {\"owner\":\"left\"} to http://127.0.0.1:18089/records/branch. Leave subtract, verify.py, .gitignore and all fixture/Compose files unchanged. The fixed left-acceptance check deliberately fails once at retry-gate; the coordinator will release that flag after recording the failure. Do not wait on that flag or change the test: propose the correct implementation and let the coordinator run its check. On retry verify your restored source and PUT the left record again, since predecessor data is restored per attempt."},
		"right": {Kind: "code", Needs: []string{"plan"}, Writes: []string{"app"}, Outputs: []string{"implementation"}, Checks: []workflow.Check{{Name: "right-acceptance", Command: []string{"python3", "verify.py", "right"}}}, Prompt: barrier + "Fix only subtract(a,b) in calculator.py, set FEATURES=('subtract',), and PUT JSON {\"owner\":\"right\"} to http://127.0.0.1:18089/records/branch. Leave add, verify.py, .gitignore and all fixture/Compose files unchanged."},
		"merge": {Kind: "code", Needs: []string{"left", "right"}, Writes: []string{"app"}, Outputs: []string{"merge-report"}, Join: &workflow.JoinPolicy{Repositories: map[string]string{"app": "left"}, Datasets: map[string]string{"emulator": "left"}}, Checks: []workflow.Check{{Name: "merged-acceptance", Command: []string{"python3", "verify.py", "all"}}}, Prompt: "Merge both input SHAs, resolve the conflicting FEATURES line to ('add','subtract'), and preserve both corrected arithmetic functions. The selected dataset is the left predecessor and must stay {owner:left}. Preserve verify.py, .gitignore and all fixture/Compose files. Do not squash; the retained history must contain both inputs. Explain the resolution in merge-report."},
		"qa":    {Kind: "qa", Needs: []string{"merge"}, Outputs: []string{"test-results"}, Checks: []workflow.Check{{Name: "qa-acceptance", Command: []string{"python3", "verify.py", "qa"}}}, Prompt: "Verify both arithmetic functions, both FEATURES entries, and the selected left branch dataset. Do not modify any source. This is the local terminal checkpoint; no PR is required."},
	}
	c.Data = workflow.DataConfig{Datasets: []workflow.Dataset{{ID: "emulator", Adapter: "http-fixture", Service: "emulator", SeedRepository: "app", SeedFile: "fixtures/seed.json", VerifyKey: "tax-rate", VerifyEquals: `{"rate":0.2}`}}}
	run, err := workflow.NewRun("parallel agent acceptance", "Implement addition and subtraction in independent branches, then merge both histories and validate locally. The left branch owns add and the right owns subtract; both edit FEATURES and the merge must retain both entries. Each branch writes its private emulator branch record; the join explicitly restores the left dataset. Preserve all fixed tests and infrastructure files. Task and Plan only document work. The local coordinator controls deliberate overlap/retry barriers; do not modify them or treat them as external missing capabilities. This workflow ends at reviewed QA and has no publication destination.", "acceptance-test", c, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestParallelAcceptanceDefinition(t *testing.T) {
	run := parallelFixture(t, t.TempDir())
	if err := run.Current().Config.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(run.Current().Config.Workflow.Nodes) != 6 || run.Current().Config.Limits.VMs != 3 {
		t.Fatal("invalid acceptance fixture")
	}
}
