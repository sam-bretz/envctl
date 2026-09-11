package localexec

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/repository"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// This injects a real guest subprocess exit before any structured proposal.
// It tests source recovery and redispatch preparation, not a model's reasoning.
func TestRealEarlyWorkerExitPreservesUncommittedSource(t *testing.T) {
	if os.Getenv("ENVCTL_RECOVERY_TEST") != "1" {
		t.Skip("opt-in real guest recovery fixture")
	}
	providerDir, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if providerDir == "" || name == "" {
		t.Fatal("select the owned acceptance VM explicitly")
	}
	b, a := fixture(t)
	b.Provider = vm.NewLima(providerDir)
	a.Revision.Runtime.ID = name
	sourceDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceDir, "work.txt"), []byte("base\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, sourceDir, "init", "-q")
	fixtureGit(t, sourceDir, "add", ".")
	fixtureGit(t, sourceDir, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "base")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	a.Revision.Config.Repositories = []workflow.Repository{{ID: "app", URL: sourceDir, Ref: "HEAD"}}
	// A writable generic node with kind task skips the Compose path, keeping
	// this fault-injection test focused on process and source durability.
	a.Attempt.Node = "task"
	node := a.Revision.Config.Workflow.Nodes["task"]
	node.Writes = []string{"app"}
	a.Revision.Config.Workflow.Nodes["task"] = node
	prepared, err := (repository.Resolver{Dir: filepath.Join(b.Store.Dir, "source")}).Prepare(ctx, a.Revision.Config.Repositories[0])
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(b.Store.Dir, "source.tar")
	if err = repository.Archive(ctx, prepared, archive); err != nil {
		t.Fatal(err)
	}
	if err = b.repos(a).Import(ctx, prepared, archive); err != nil {
		t.Fatal(err)
	}
	a.Revision.SourcePins = map[string]string{"app": prepared.Repository.BaseSHA}
	if err = b.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	r, err := b.load(a)
	if err != nil {
		t.Fatal(err)
	}
	_, err = b.guest(a).Submit(ctx, guestjob.Request{ID: r.Worker.ID, Args: []string{"python3", "-c", "open('work.txt','w').write('unfinished edit\\n'); open('untracked.txt','w').write('new file\\n'); raise SystemExit(7)"}, Dir: r.Directories["app"], TimeoutSeconds: 20})
	if err != nil {
		t.Fatal(err)
	}
	var observation engine.Observation
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline); {
		observation, err = b.Poll(ctx, a)
		if err != nil {
			t.Fatal(err)
		}
		if observation.State == "failed" {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if observation.State != "failed" || observation.Result != nil {
		t.Fatal("early exit fabricated a proposal")
	}
	r, err = b.load(a)
	if err != nil || r.Recovery == nil || len(r.Recovery.Sources) != 1 || r.Recovery.Commits["app"] == prepared.Repository.BaseSHA {
		t.Fatal("unfinished work not retained", err)
	}
	if len(r.Result.Commits) != 0 || r.Result.Review.Accepted {
		t.Fatal("recovery promoted unreviewed output")
	}
	prior := a.Attempt
	prior.State = "failed"
	prior.Error = observation.Detail
	a.Revision.Attempts = []workflow.Attempt{prior}
	oldWorktree := r.Directories["app"]
	sourceGuest, _ := b.repos(a).SourceDir("app")
	if err = b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "git", "-C", sourceGuest, "worktree", "remove", "--force", oldWorktree}}); err != nil {
		t.Fatal(err)
	}
	a.Attempt = workflow.Attempt{ID: "attempt_retry", Node: "task", Number: 2, Inputs: workflow.Clone(prior.Inputs)}
	b = New(b.Store)
	b.Provider = vm.NewLima(providerDir)
	if err = b.Start(ctx, a); err != nil {
		t.Fatal(err)
	}
	retry, err := b.load(a)
	if err != nil || retry.RecoveredFrom != prior.ID || !strings.Contains(retry.Worker.Prompt, "unfinished work") {
		t.Fatal("retry lineage/context missing", err)
	}
	var files bytes.Buffer
	if err = b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "cat", retry.Directories["app"] + "/work.txt", retry.Directories["app"] + "/untracked.txt"}, Stdout: &files}); err != nil {
		t.Fatal(err)
	}
	if files.String() != "unfinished edit\nnew file\n" {
		t.Fatal("recovered tracked/untracked bytes differ")
	}
	original, err := os.ReadFile(filepath.Join(sourceDir, "work.txt"))
	if err != nil || string(original) != "base\n" {
		t.Fatal("host source changed", err)
	}
	if retry.Worker.Session != "" {
		t.Fatal("invented a model session for subprocess fixture")
	}
	if err = b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", "/work/envctl/revisions/" + a.Revision.ID, "/var/lib/envctl/repository-receipts/" + a.Revision.ID}}); err != nil {
		t.Fatal("source fixture cleanup", err)
	}
	t.Log("early exit retained tracked/untracked source, no checkpoint, backend restart and new-attempt restoration passed")
}
