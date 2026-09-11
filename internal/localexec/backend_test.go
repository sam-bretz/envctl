package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type jobFixture struct {
	vm.Provider
	states    map[string]string
	cancelled []string
}

func (f *jobFixture) Exec(_ context.Context, _ string, c vm.Command) error {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(c.Stdin).Decode(&req); err != nil {
		return err
	}
	state := f.states[req.ID]
	if state == "" {
		state = "missing"
	}
	if c.Args[len(c.Args)-1] == "cancel" {
		if state == "missing" {
			return errors.New("unknown job")
		}
		f.cancelled = append(f.cancelled, req.ID)
		state = "cancelled"
		f.states[req.ID] = state
	}
	return json.NewEncoder(c.Stdout).Encode(map[string]any{"id": req.ID, "state": state, "cursor": 0, "truncated": false})
}

func TestCancelHandlesPartiallyDispatchedAssignments(t *testing.T) {
	b, a := fixture(t)
	f := &jobFixture{states: map[string]string{a.Attempt.ID + "_worker": "running"}}
	b.Provider = f
	if err := b.Cancel(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if len(f.cancelled) != 1 || f.cancelled[0] != a.Attempt.ID+"_worker" {
		t.Fatal("wrong jobs cancelled", f.cancelled)
	}
	if err := b.Cancel(context.Background(), a); err != nil {
		t.Fatal("cancellation not repeatable", err)
	}
	if len(f.cancelled) != 1 {
		t.Fatal("terminal job unnecessarily cancelled")
	}
}

func fixture(t *testing.T) (*Backend, engine.Assignment) {
	t.Helper()
	s, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := workflow.NewRun("feature", "fix arithmetic", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.Current().SourcePins = map[string]string{"app": "base"}
	r.Current().Runtime.ID = "envctl-test"
	return New(s), engine.Assignment{Run: *r, Revision: *r.Current(), Attempt: workflow.Attempt{ID: workflow.ID("attempt"), Node: "code", Inputs: map[string]string{}}}
}

func TestHarnessCredentialsAreRedactedFromDurableArtifacts(t *testing.T) {
	b, a := fixture(t)
	t.Setenv("ENVCTL_REDACTION_FIXTURE", "credential-with-newline\nsecret")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_REDACTION_FIXTURE"
	a.Revision.Config.Agents.Supervisor.Credential = "env:ENVCTL_REDACTION_FIXTURE"
	artifact, err := b.artifact(a, "review", "application/json", mustJSON(map[string]string{"summary": os.Getenv("ENVCTL_REDACTION_FIXTURE")}))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := b.Store.Artifact(artifact.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "credential") || !strings.Contains(string(raw), "[REDACTED]") {
		t.Fatalf("credential not removed: %s", raw)
	}
	if _, err = b.artifact(a, "source.app.bundle", "application/x-git-bundle", []byte(os.Getenv("ENVCTL_REDACTION_FIXTURE"))); err == nil {
		t.Fatal("binary artifact with credential accepted")
	}
}
func TestInputLineageRejectsConflictingJoins(t *testing.T) {
	_, a := fixture(t)
	a.Attempt.Inputs = map[string]string{"left": "cp_left", "right": "cp_right"}
	a.Revision.Checkpoints = map[string]workflow.Checkpoint{
		"left":  {ID: "cp_left", Result: workflow.Result{Commits: map[string]string{"app": "left"}}},
		"right": {ID: "cp_right", Result: workflow.Result{Commits: map[string]string{"app": "right"}}},
	}
	if _, err := inputCommits(a); err == nil {
		t.Fatal("conflicting join silently selected one writer")
	}
	right := a.Revision.Checkpoints["right"]
	right.Result.Commits["app"] = "left"
	a.Revision.Checkpoints["right"] = right
	pins, err := inputCommits(a)
	if err != nil || pins["app"] != "left" {
		t.Fatal("matching lineage rejected", err)
	}
	a.Attempt.Inputs["left"] = "stale"
	if _, err = inputCommits(a); err == nil {
		t.Fatal("stale checkpoint accepted")
	}
}

func TestParallelWorkerCannotFallBackToRevisionRuntime(t *testing.T) {
	b, a := fixture(t)
	a.Revision.Config.Limits.Parallel = 2
	if err := b.Start(context.Background(), a); err == nil {
		t.Fatal("parallel worker used parent stack")
	}
	if _, err := b.Poll(context.Background(), a); err == nil {
		t.Fatal("parallel worker polled parent stack")
	}
	a.Child = a.Attempt.Node
	a.Revision.ChildRuntimes = map[string]*workflow.ChildRuntime{a.Child: {Runtime: workflow.RuntimeState{ID: "envctl-child", Ready: true}}}
	a.Revision.Runtime = workflow.RuntimeState{ID: "envctl-child", Ready: true}
	if err := assignmentRuntime(a); err != nil {
		t.Fatal(err)
	}
	a.Child = "sibling"
	if err := assignmentRuntime(a); err == nil {
		t.Fatal("worker borrowed sibling namespace")
	}
}

func TestReadinessUsesSelectedCheckpointSource(t *testing.T) {
	_, a := fixture(t)
	a.Revision.SourcePins = map[string]string{"app": strings.Repeat("a", 40)}
	a.Revision.Checkpoints["code"] = workflow.Checkpoint{ID: "cp_code", Result: workflow.Result{Commits: map[string]string{"app": strings.Repeat("b", 40)}}}
	a.Revision.FromCheckpoint = "cp_code"
	pins, err := readinessCommits(a)
	if err != nil || pins["app"] != strings.Repeat("b", 40) {
		t.Fatal("restoration probes the baseline instead of selected code", err)
	}
	pins["app"] = "mutated"
	if a.Revision.Checkpoints["code"].Result.Commits["app"] != strings.Repeat("b", 40) {
		t.Fatal("readiness mutated immutable lineage")
	}
	cp := a.Revision.Checkpoints["code"]
	cp.HistoricalOnly = true
	a.Revision.Checkpoints["code"] = cp
	if _, err := readinessCommits(a); err == nil {
		t.Fatal("historical checkpoint supplied runnable source")
	}
	cp.HistoricalOnly = false
	cp.Result.Commits = map[string]string{}
	a.Revision.Checkpoints["code"] = cp
	if _, err := readinessCommits(a); err == nil {
		t.Fatal("incomplete checkpoint fell back to unverified baseline")
	}
	a.Revision.FromCheckpoint = "missing"
	if _, err := readinessCommits(a); err == nil {
		t.Fatal("missing selected checkpoint ignored")
	}
}
func TestAttemptReceiptSurvivesReconstructionAndRejectsRebinding(t *testing.T) {
	b, a := fixture(t)
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "checks", Session: "explicit-session", CheckIndex: 1, Logs: map[string]string{"check": "evidence"}}
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	fresh := New(b.Store)
	got, err := fresh.load(a)
	if err != nil || got.Session != r.Session || got.CheckIndex != 1 {
		t.Fatal("receipt not recovered", err)
	}
	got.Result.Sources["app"] = workflow.Artifact{Name: "source"} // empty omitted maps remain writable after recovery
	p, _ := b.recordPath(a)
	info, err := os.Stat(p)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("receipt is not private")
	}
	a.Revision.Objective = "different task"
	if _, err = fresh.load(a); err == nil {
		t.Fatal("receipt rebound to a different objective")
	}
	a.Attempt.ID = "../escape"
	if _, err = fresh.load(a); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatal("unsafe attempt path accepted")
	}
}
