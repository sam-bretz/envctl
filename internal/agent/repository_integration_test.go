package agent

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/repository"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Runs inside the already-provisioned acceptance VM, using separate directories
// from the harness fixture. This tests actual Git worktrees and bundle restore.
func exerciseGuestRepositories(t *testing.T, ctx context.Context, provider *vm.Lima, runtimeID string) {
	t.Helper()
	root := t.TempDir()
	nonce := fmt.Sprintf("%d", time.Now().UnixNano())
	guest := repository.Guest{Executor: provider, Runtime: runtimeID, Revision: "rev_repository_fixture_" + nonce}
	restored := repository.Guest{Executor: provider, Runtime: runtimeID, Revision: "rev_repository_restored_" + nonce}
	for _, id := range []string{"app", "service"} {
		sourceDir := filepath.Join(root, id)
		if err := os.MkdirAll(sourceDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(sourceDir, "value.txt"), []byte("base "+id), 0600); err != nil {
			t.Fatal(err)
		}
		git := func(args ...string) {
			t.Helper()
			cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "commit.gpgSign=false", "-C", sourceDir}, args...)...)
			if b, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture Git: %v: %s", err, b)
			}
		}
		git("init", "-q")
		git("config", "user.name", "envctl fixture")
		git("config", "user.email", "envctl-fixture@example.invalid")
		git("add", "value.txt")
		git("commit", "-qm", "base fixture")
		source, err := (repository.Resolver{Dir: filepath.Join(root, "prepared")}).Prepare(ctx, workflow.Repository{ID: id, URL: sourceDir, Ref: "HEAD"})
		if err != nil {
			t.Fatal(err)
		}
		archive := filepath.Join(root, id+".tar")
		if err = repository.Archive(ctx, source, archive); err != nil {
			t.Fatal(err)
		}
		if err = guest.Import(ctx, source, archive); err != nil {
			t.Fatal(err)
		}
		if err = guest.Import(ctx, source, archive); err != nil {
			t.Fatal("repeat import", err)
		}
		left, err := guest.Assign(ctx, "attempt_left", id, source.Repository.BaseSHA)
		if err != nil {
			t.Fatal(err)
		}
		right, err := guest.Assign(ctx, "attempt_right", id, source.Repository.BaseSHA)
		if err != nil {
			t.Fatal(err)
		}
		if err = provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "sh", "-c", `printf '%s' "$2" > "$1/value.txt"`, "write-fixture", left, "changed " + id}}); err != nil {
			t.Fatal(err)
		}
		if _, err = guest.Assign(ctx, "attempt_left", id, source.Repository.BaseSHA); err != nil {
			t.Fatal("assignment retry reset live files", err)
		}
		var out strings.Builder
		if err = provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "cat", right + "/value.txt"}, Stdout: &out}); err != nil {
			t.Fatal(err)
		}
		if out.String() != "base "+id {
			t.Fatal("parallel writer changed sibling files")
		}
		if _, err = guest.Capture(ctx, "attempt_left", id, false); err == nil {
			t.Fatal("read-only stage accepted edits")
		}
		commit, err := guest.Capture(ctx, "attempt_left", id, true)
		if err != nil {
			t.Fatal(err)
		}
		if commit == source.Repository.BaseSHA {
			t.Fatal("checkpoint has no changed commit")
		}
		var bundle bytes.Buffer
		if err = guest.ExportBundle(ctx, "attempt_left", id, commit, &bundle); err != nil {
			t.Fatal(err)
		}
		if err = restored.Import(ctx, source, archive); err != nil {
			t.Fatal(err)
		}
		if err = restored.RestoreBundle(ctx, id, commit, bytes.NewReader(bundle.Bytes())); err != nil {
			t.Fatal(err)
		}
		destination, err := restored.Assign(ctx, "attempt_restore", id, commit)
		if err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err = provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "cat", destination + "/value.txt"}, Stdout: &out}); err != nil {
			t.Fatal(err)
		}
		if out.String() != "changed "+id {
			t.Fatal("bundle restore lost checkpoint content")
		}
		original, err := os.ReadFile(filepath.Join(sourceDir, "value.txt"))
		if err != nil || string(original) != "base "+id {
			t.Fatal("guest modified host source", err)
		}
	}
	t.Log("two repositories imported at immutable pins; independent writer worktrees, retry preservation, exact commit bundles, and restoration verified")
}
