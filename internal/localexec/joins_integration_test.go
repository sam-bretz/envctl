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

// A real guest Git fixture exercises divergent source retention, offline join
// preparation/replay and independent coordinator verification. It does not
// claim concurrent child VMs or model reasoning; those have separate exits.
func TestRealGuestVerifiedJoinAndReplay(t *testing.T) {
	if os.Getenv("ENVCTL_JOIN_TEST") != "1" {
		t.Skip("opt-in real guest merge acceptance")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("select the owned acceptance VM explicitly")
	}
	b, a := fixture(t)
	b.Provider = vm.NewLima(state)
	a.Revision.Runtime.ID = name
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	t.Cleanup(func() {
		clean, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := b.Provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", "/work/envctl/revisions/" + a.Revision.ID, "/var/lib/envctl/repository-receipts/" + a.Revision.ID}}); err != nil {
			t.Error("join fixture cleanup", err)
		}
	})
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "work.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixtureGit(t, root, "init", "-q")
	fixtureGit(t, root, "add", ".")
	fixtureGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "-qm", "base")
	a.Revision.Config.Repositories = []workflow.Repository{{ID: "app", URL: root, Ref: "HEAD"}}
	source, err := (repository.Resolver{Dir: filepath.Join(b.Store.Dir, "source")}).Prepare(ctx, a.Revision.Config.Repositories[0])
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(b.Store.Dir, "source.tar")
	if err := repository.Archive(ctx, source, archive); err != nil {
		t.Fatal(err)
	}
	if err := b.repos(a).Import(ctx, source, archive); err != nil {
		t.Fatal(err)
	}
	a.Revision.SourcePins = map[string]string{"app": source.Repository.BaseSHA}
	a.Revision.Checkpoints = map[string]workflow.Checkpoint{}
	guest := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := b.Provider.Exec(ctx, name, vm.Command{Args: append([]string{"sudo", "-u", "envctl-agent"}, args...), Stdout: &out})
		return strings.TrimSpace(out.String()), err
	}
	write := func(directory, body string) {
		t.Helper()
		if _, err := guest("python3", "-c", "import pathlib,sys; (pathlib.Path(sys.argv[1])/'work.txt').write_text(sys.argv[2])", directory, body); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(attempt string) workflow.Result {
		t.Helper()
		pin, err := b.repos(a).Capture(ctx, attempt, "app", true)
		if err != nil {
			t.Fatal(err)
		}
		var bundle bytes.Buffer
		if err := b.repos(a).ExportBundle(ctx, attempt, "app", pin, &bundle); err != nil {
			t.Fatal(err)
		}
		artifact, err := b.Store.PutArtifact("source", "application/x-git-bundle", bundle.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		var objects bytes.Buffer
		if err := b.repos(a).ExportObjects(ctx, attempt, "app", pin, &objects); err != nil {
			t.Fatal(err)
		}
		companion, err := b.Store.PutArtifact("source-objects", repository.ObjectsMediaType, objects.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return workflow.Result{Commits: map[string]string{"app": pin}, Sources: map[string]workflow.Artifact{"app": artifact}, SourceObjects: map[string]workflow.Artifact{"app": companion}}
	}
	for _, parent := range []string{"left", "right"} {
		directory, err := b.repos(a).Assign(ctx, parent, "app", source.Repository.BaseSHA)
		if err != nil {
			t.Fatal(err)
		}
		write(directory, parent+"\n")
		a.Revision.Checkpoints[parent] = workflow.Checkpoint{ID: "cp_" + parent, Result: capture(parent)}
	}
	// Inputs survive loss of both original source and writer worktrees.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	for _, parent := range []string{"left", "right"} {
		directory, _ := b.repos(a).WorktreeDir(parent, "app")
		sourceDirectory, err := b.repos(a).SourceDir("app")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := guest("git", "-C", sourceDirectory, "worktree", "remove", "--force", directory); err != nil {
			t.Fatal(err)
		}
	}
	a.Attempt.Node = "merge"
	a.Attempt.Inputs = map[string]string{"left": "cp_left", "right": "cp_right"}
	a.Revision.Config.Workflow.Nodes["merge"] = workflow.Node{Kind: "code", Needs: []string{"left", "right"}, Writes: []string{"app"}, Outputs: []string{"merged"}, Checks: []workflow.Check{{Name: "contents", Command: []string{"make", "test"}}}, Join: &workflow.JoinPolicy{Repositories: map[string]string{"app": "left"}}}
	pins, err := inputCommits(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.prepareJoin(ctx, a); err != nil {
		t.Fatal(err)
	}
	dirs, err := b.worktrees(ctx, a, a.Attempt.ID, pins)
	if err != nil {
		t.Fatal(err)
	}
	unmerged := capture(a.Attempt.ID)
	if err := b.verifyJoin(ctx, a, &unmerged); err == nil {
		t.Fatal("unmerged output accepted")
	}
	right := a.Revision.Checkpoints["right"].Result.Commits["app"]
	if _, err := guest("git", "-C", dirs["app"], "merge", "--no-edit", right); err == nil {
		t.Fatal("fixture did not conflict")
	}
	write(dirs["app"], "left and right\n")
	// Replay preparation across backend recreation preserves live resolution.
	b = &Backend{Store: b.Store, Provider: b.Provider}
	if err := b.prepareJoin(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := b.worktrees(ctx, a, a.Attempt.ID, pins); err != nil {
		t.Fatal(err)
	}
	if body, err := guest("cat", dirs["app"]+"/work.txt"); err != nil || body != "left and right" {
		t.Fatal("replay overwrote resolution", err)
	}
	merged := capture(a.Attempt.ID)
	if err := b.verifyJoin(ctx, a, &merged); err != nil {
		t.Fatal(err)
	}
	if len(merged.MergeParents["app"]) != 2 {
		t.Fatal("merge lost parent identities")
	}
	for _, parent := range []string{"left", "right"} {
		directory, _ := b.repos(a).WorktreeDir(joinInputID(a, parent), "app")
		if body, err := guest("cat", directory+"/work.txt"); err != nil || body != parent {
			t.Fatal("merge changed input copy", parent, err)
		}
	}
	t.Log("retained divergent inputs, real conflict, replay preservation, merged ancestry and independent input copies verified")
}
