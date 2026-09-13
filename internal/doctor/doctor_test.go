package doctor

import (
	"context"
	"testing"
	"time"
)

func TestReportAggregateFailed(t *testing.T) {
	dir := t.TempDir()

	t.Run("ok and warnings only", func(t *testing.T) {
		stubs := allOKStubs(dir)
		// No git repository, no Lima, no gh auth: all Warnings, must not fail
		// the report or stop the rest of it from running.
		stubs[cmdKey("git", "-C", dir, "rev-parse", "--show-toplevel")] = stub{code: 128, stderr: "fatal: not a git repository"}
		stubs[cmdKey("limactl", "--version")] = stub{code: 1}
		stubs[cmdKey("gh", "auth", "status")] = stub{code: 1, stderr: "not logged in"}
		env := Env{Run: stubRunner(t, stubs), Dir: dir, GOOS: "linux"}
		results, failed := Report(context.Background(), env)
		if failed {
			t.Fatalf("warnings alone must not fail the report: %+v", results)
		}
		sawWarning := false
		for _, r := range results {
			if r.Status == Failed {
				t.Fatalf("unexpected failure: %+v", r)
			}
			if r.Status == Warning {
				sawWarning = true
			}
		}
		if !sawWarning {
			t.Fatal("expected at least one warning")
		}
	})

	t.Run("any failure fails the report", func(t *testing.T) {
		stubs := allOKStubs(dir)
		stubs[cmdKey("docker", "info", "--format", "{{.OperatingSystem}}")] = stub{code: 1, stderr: "Cannot connect to the Docker daemon"}
		env := Env{Run: stubRunner(t, stubs), Dir: dir, GOOS: "linux"}
		_, failed := Report(context.Background(), env)
		if !failed {
			t.Fatal("expected a failure")
		}
	})
}

// A canceled context must be scored at each check's own severity rather than
// panicking or hanging; this uses no real sleep so the test stays fast.
func TestReportHandlesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	run := func(ctx context.Context, _ time.Duration, name string, args ...string) (string, string, int, error) {
		if err := ctx.Err(); err != nil {
			return "", "", -1, err
		}
		return "", "", 0, nil
	}
	env := Env{Run: run, Dir: t.TempDir(), GOOS: "linux"}
	results, failed := Report(ctx, env)
	if !failed {
		t.Fatal("expected the canceled docker/git checks to fail")
	}
	for _, r := range results {
		if r.Name == "docker" && r.Status != Failed {
			t.Fatalf("expected docker to fail under a canceled context, got %+v", r)
		}
	}
}
