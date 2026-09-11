package repository

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealGuestNestedSubmoduleAndLFSCheckpointReplay(t *testing.T) {
	if os.Getenv("ENVCTL_SOURCE_OBJECTS_TEST") != "1" {
		t.Skip("opt-in real guest submodule/LFS restoration")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("explicit owned guest required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	root, origins := nestedObjectRepo(t)
	pin := objectCommand(t, root, "rev-parse", "HEAD")
	source := Source{Repository: workflow.Repository{ID: "app", URL: root, BaseSHA: pin}, Checkout: root, LFS: true}
	archive := filepath.Join(t.TempDir(), "baseline.tar")
	if err := Archive(ctx, source, archive); err != nil {
		t.Fatal(err)
	}
	for _, origin := range origins {
		if err := os.RemoveAll(origin); err != nil {
			t.Fatal(err)
		}
	}
	provider := vm.NewLima(state)
	createGuest := func() Guest {
		g := Guest{Executor: provider, Runtime: name, Revision: workflow.ID("rev")}
		t.Cleanup(func() {
			clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
			defer stop()
			if err := provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", "/work/envctl/revisions/" + g.Revision, "/var/lib/envctl/repository-receipts/" + g.Revision}}); err != nil {
				t.Error("source fixture cleanup", err)
			}
		})
		if err := g.Import(ctx, source, archive); err != nil {
			t.Fatal(err)
		}
		return g
	}
	first := createGuest()
	attempt := workflow.ID("attempt")
	directory, err := first.Assign(ctx, attempt, "app", pin)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(directory, leaf string) {
		t.Helper()
		var raw bytes.Buffer
		script := `import json,pathlib,sys
r=pathlib.Path(sys.argv[1]);print(json.dumps([ (r/p).read_bytes().decode() for p in ['data.bin','vendor/middle module/data.bin','vendor/middle module/leaf module/data.bin']]))`
		if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "python3", "-c", script, directory}, Stdout: &raw}); err != nil {
			t.Fatal(err)
		}
		var values []string
		if err := json.Unmarshal(raw.Bytes(), &values); err != nil || len(values) != 3 || values[0] != "root baseline\x00" || values[1] != "middle baseline\x00" || values[2] != leaf {
			t.Fatal("guest LFS data differs", err)
		}
	}
	verify(directory, "leaf baseline\x00")
	independent := workflow.ID("attempt")
	other, err := first.Assign(ctx, independent, "app", pin)
	if err != nil {
		t.Fatal(err)
	}
	change := `import pathlib,sys
(pathlib.Path(sys.argv[1])/'vendor/middle module/leaf module/data.bin').write_bytes(b'changed in guest\x00')`
	if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "python3", "-c", change, directory}}); err != nil {
		t.Fatal(err)
	}
	// Submission replay preserves the original writer's uncommitted changes.
	if _, err := first.Assign(ctx, attempt, "app", pin); err != nil {
		t.Fatal(err)
	}
	verify(directory, "changed in guest\x00")
	verify(other, "leaf baseline\x00")
	if _, err := first.Capture(ctx, attempt, "app", false); err == nil {
		t.Fatal("read-only submodule edit accepted")
	}
	changed, err := first.Capture(ctx, attempt, "app", true)
	if err != nil || changed == pin {
		t.Fatal("nested guest checkpoint capture", err)
	}
	var bundle, objects bytes.Buffer
	if err := first.ExportBundle(ctx, attempt, "app", changed, &bundle); err != nil {
		t.Fatal(err)
	}
	if err := first.ExportObjects(ctx, attempt, "app", changed, &objects); err != nil {
		t.Fatal(err)
	}
	// Delete the entire first revision before constructing fresh source state.
	if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", "/work/envctl/revisions/" + first.Revision, "/var/lib/envctl/repository-receipts/" + first.Revision}}); err != nil {
		t.Fatal(err)
	}
	restored := createGuest()
	if err := restored.RestoreBundle(ctx, "app", changed, bytes.NewReader(bundle.Bytes())); err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreObjects(ctx, "app", changed, bytes.NewReader(objects.Bytes())); err != nil {
		t.Fatal(err)
	}
	cache, err := restored.objectsPath("app", changed)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "python3", "-c", "import pathlib,sys;pathlib.Path(sys.argv[1]).write_bytes(b'corrupt guest cache')", cache}}); err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreObjects(ctx, "app", changed, bytes.NewReader(objects.Bytes())); err != nil {
		t.Fatal("retained companion could not repair damaged guest cache", err)
	}
	restoredAttempt := workflow.ID("attempt")
	target, err := restored.Assign(ctx, restoredAttempt, "app", changed)
	if err != nil {
		t.Fatal(err)
	}
	verify(target, "changed in guest\x00")
	if got, err := restored.Capture(ctx, restoredAttempt, "app", false); err != nil || got != changed {
		t.Fatal("restored source is not exact and clean", err)
	}
	t.Log("real guest nested modules and LFS, independent assignment source, replay without resetting live edits, read-only rejection, recursive commit capture and restoration after deleting originals/first revision passed")
}
