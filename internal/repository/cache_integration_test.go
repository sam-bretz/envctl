package repository

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealRetainedBaselineGuestImport(t *testing.T) {
	if os.Getenv("ENVCTL_REPOSITORY_CACHE_TEST") != "1" {
		t.Skip("opt-in retained archive transfer to an owned guest")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("explicit owned acceptance VM required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	origin := sourceRepo(t)
	prepared := t.TempDir()
	cache := Cache{Dir: t.TempDir()}
	source, archive, err := cache.Prepare(ctx, Resolver{Dir: prepared}, workflow.Repository{ID: "app", URL: origin, Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	pin := source.Repository.BaseSHA
	if err = os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(prepared); err != nil {
		t.Fatal(err)
	}
	provider := vm.NewLima(state)
	for i := 0; i < 2; i++ {
		source, again, err := cache.Prepare(ctx, Resolver{Dir: filepath.Join(t.TempDir(), "unused")}, source.Repository)
		if err != nil || again != archive {
			t.Fatal("offline source retention failed", err)
		}
		g := Guest{Executor: provider, Runtime: name, Revision: workflow.ID("rev")}
		t.Cleanup(func() {
			clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			if err := provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", "/work/envctl/revisions/" + g.Revision, "/var/lib/envctl/repository-receipts/" + g.Revision}}); err != nil {
				t.Error("scoped cache fixture cleanup", err)
			}
		})
		if err = g.Import(ctx, source, again); err != nil {
			t.Fatal(err)
		}
		attempt := workflow.ID("attempt")
		dir, err := g.Assign(ctx, attempt, "app", pin)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err = provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "sh", "-c", "git -C \"$1\" rev-parse HEAD; cat \"$1/file.txt\"", "fixture", dir}, Stdout: &out}); err != nil || strings.TrimSpace(out.String()) != pin+"\nfirst" {
			t.Fatal("retained source imported incorrectly", err)
		}
		if i == 0 {
			if err = provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "sh", "-c", "printf changed > \"$1/file.txt\"", "fixture", dir}}); err != nil {
				t.Fatal(err)
			}
			changed, err := g.Capture(ctx, attempt, "app", true)
			if err != nil || changed == pin {
				t.Fatal("guest change did not produce independent commit", err)
			}
		}
	}
	t.Log("two scoped guest imports from retained baseline after source/preparation deletion; first guest mutation did not alter the second baseline; no model or remote fetch used")
}
