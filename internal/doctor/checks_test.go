package doctor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

type stub struct {
	stdout, stderr string
	code           int
	err            error
}

func cmdKey(name string, args ...string) string {
	return strings.Join(append([]string{name}, args...), "\x00")
}

func stubRunner(t *testing.T, stubs map[string]stub) Runner {
	t.Helper()
	return func(_ context.Context, _ time.Duration, name string, args ...string) (string, string, int, error) {
		key := cmdKey(name, args...)
		s, ok := stubs[key]
		if !ok {
			t.Fatalf("unexpected command: %s %v", name, args)
		}
		return s.stdout, s.stderr, s.code, s.err
	}
}

func TestCheckDocker(t *testing.T) {
	ctx := context.Background()
	t.Run("unreachable", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{
			cmdKey("docker", "info", "--format", "{{.OperatingSystem}}"): {code: 1, stderr: "Cannot connect to the Docker daemon"},
		})}
		results := checkDocker(ctx, env)
		if len(results) != 1 || results[0].Status != Failed {
			t.Fatalf("expected a single Failed result, got %+v", results)
		}
	})
	t.Run("compose v1 rejected", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{
			cmdKey("docker", "info", "--format", "{{.OperatingSystem}}"): {code: 0},
			cmdKey("docker", "compose", "version", "--short"):            {code: 0, stdout: "1.29.2\n"},
		})}
		results := checkDocker(ctx, env)
		if len(results) != 2 || results[0].Status != OK || results[1].Status != Failed {
			t.Fatalf("expected compose v1 to fail, got %+v", results)
		}
	})
	// A healthy install must be accepted regardless of whether --short prints
	// a "v" prefix, and regardless of major version as long as it is >= 2:
	// Compose 5.1.3 failing here was the exact regression reported on a real
	// machine.
	for _, tc := range []struct{ raw, wantDetail string }{
		{"v2.29.0\n", "docker compose 2.29.0"},
		{"2.29.0\n", "docker compose 2.29.0"},
		{"5.1.3\n", "docker compose 5.1.3"},
	} {
		t.Run("compose "+tc.raw, func(t *testing.T) {
			env := Env{Run: stubRunner(t, map[string]stub{
				cmdKey("docker", "info", "--format", "{{.OperatingSystem}}"): {code: 0},
				cmdKey("docker", "compose", "version", "--short"):            {code: 0, stdout: tc.raw},
			})}
			results := checkDocker(ctx, env)
			if len(results) != 2 || results[0].Status != OK || results[1].Status != OK {
				t.Fatalf("expected two OK results, got %+v", results)
			}
			if results[1].Detail != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", results[1].Detail, tc.wantDetail)
			}
		})
	}
}

func TestCheckGit(t *testing.T) {
	ctx := context.Background()
	env := Env{Run: stubRunner(t, map[string]stub{
		cmdKey("git", "--version"): {code: 0, stdout: "git version 2.43.0\n"},
	})}
	if r := checkGit(ctx, env); r.Status != OK || r.Detail != "git version 2.43.0" {
		t.Fatalf("unexpected result: %+v", r)
	}
	env = Env{Run: stubRunner(t, map[string]stub{
		cmdKey("git", "--version"): {err: context.DeadlineExceeded, code: -1},
	})}
	if r := checkGit(ctx, env); r.Status != Failed {
		t.Fatalf("expected Failed, got %+v", r)
	}
}

func TestCheckLima(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		stub   stub
		status Status
	}{
		{"missing", stub{err: os.ErrNotExist, code: -1}, Warning},
		{"v1", stub{code: 0, stdout: "limactl version 1.0.0\n"}, Warning},
		{"v2", stub{code: 0, stdout: "limactl version 2.1.0\n"}, OK},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := Env{Run: stubRunner(t, map[string]stub{
				cmdKey("limactl", "--version"): tc.stub,
			})}
			if r := checkLima(ctx, env); r.Status != tc.status {
				t.Fatalf("status = %s, want %s (%+v)", r.Status, tc.status, r)
			}
		})
	}
}

func TestCheckGH(t *testing.T) {
	ctx := context.Background()
	env := Env{Run: stubRunner(t, map[string]stub{
		cmdKey("gh", "auth", "status"): {err: os.ErrNotExist, code: -1},
	})}
	if r := checkGH(ctx, env); r.Status != Warning || r.Detail != "gh is not installed" {
		t.Fatalf("unexpected result: %+v", r)
	}
	env = Env{Run: stubRunner(t, map[string]stub{
		cmdKey("gh", "auth", "status"): {code: 1, stderr: "You are not logged into any GitHub hosts"},
	})}
	if r := checkGH(ctx, env); r.Status != Warning || r.Detail != "You are not logged into any GitHub hosts" {
		t.Fatalf("unexpected result: %+v", r)
	}
	env = Env{Run: stubRunner(t, map[string]stub{
		cmdKey("gh", "auth", "status"): {code: 0},
	})}
	if r := checkGH(ctx, env); r.Status != OK {
		t.Fatalf("unexpected result: %+v", r)
	}
}

func gitToplevelStub(root string, ok bool) map[string]stub {
	if !ok {
		return map[string]stub{cmdKey("git", "-C", root, "rev-parse", "--show-toplevel"): {code: 128, stderr: "fatal: not a git repository"}}
	}
	return map[string]stub{cmdKey("git", "-C", root, "rev-parse", "--show-toplevel"): {code: 0, stdout: root + "\n"}}
}

func writeV1Manifest(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestYAML := "version: 1\nproject: mg\nstack:\n  files: [docker-compose.yml]\n"
	if err := os.WriteFile(filepath.Join(dir, "envctl.yaml"), []byte(manifestYAML), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeV2Manifest(t *testing.T, dir, workerCred, supervisorCred string) {
	t.Helper()
	doc := "version: 2\nproject: mg\nrepositories:\n  - id: main\n    url: https://example.com/repo.git\nworkflow:\n  template: feature\nagents:\n  worker:\n    kind: claude\n    credential: " + workerCred + "\n  supervisor:\n    kind: claude\n    credential: " + supervisorCred + "\n"
	if err := os.WriteFile(filepath.Join(dir, "envctl.yaml"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckManifest(t *testing.T) {
	ctx := context.Background()

	t.Run("not a git repo", func(t *testing.T) {
		dir := t.TempDir()
		env := Env{Run: stubRunner(t, gitToplevelStub(dir, false)), Dir: dir}
		r, cfg, v := checkManifest(ctx, env)
		if r.Status != Warning || cfg != nil || v != 0 || !strings.Contains(r.Detail, "not inside a git repository") {
			t.Fatalf("unexpected result: %+v cfg=%v v=%d", r, cfg, v)
		}
	})

	t.Run("no manifest", func(t *testing.T) {
		dir := t.TempDir()
		env := Env{Run: stubRunner(t, gitToplevelStub(dir, true)), Dir: dir}
		r, cfg, v := checkManifest(ctx, env)
		if r.Status != Warning || cfg != nil || v != 0 || !strings.Contains(r.Detail, "envctl.yaml not found") {
			t.Fatalf("unexpected result: %+v cfg=%v v=%d", r, cfg, v)
		}
	})

	t.Run("v1 manifest", func(t *testing.T) {
		dir := t.TempDir()
		writeV1Manifest(t, dir)
		env := Env{Run: stubRunner(t, gitToplevelStub(dir, true)), Dir: dir}
		r, cfg, v := checkManifest(ctx, env)
		if r.Status != OK || !strings.Contains(r.Detail, "version 1") {
			t.Fatalf("unexpected result: %+v", r)
		}
		// Regression: workflow.Load always reports Config.Version 2, even for
		// a v1 manifest on disk. The raw on-disk version must still be 1.
		if v != 1 {
			t.Fatalf("rawVersion = %d, want 1", v)
		}
		if cfg == nil || cfg.Version != 2 {
			t.Fatalf("expected workflow.Load's normalized Config.Version 2, got %+v", cfg)
		}
	})

	t.Run("v2 manifest", func(t *testing.T) {
		dir := t.TempDir()
		writeV2Manifest(t, dir, "env:MG_WORKER_CRED", "env:MG_SUPERVISOR_CRED")
		env := Env{Run: stubRunner(t, gitToplevelStub(dir, true)), Dir: dir}
		r, cfg, v := checkManifest(ctx, env)
		if r.Status != OK || !strings.Contains(r.Detail, "version 2") || v != 2 || cfg == nil {
			t.Fatalf("unexpected result: %+v cfg=%v v=%d", r, cfg, v)
		}
	})

	t.Run("malformed yaml", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "envctl.yaml"), []byte("version: [this is not valid"), 0o644); err != nil {
			t.Fatal(err)
		}
		env := Env{Run: stubRunner(t, gitToplevelStub(dir, true)), Dir: dir}
		r, cfg, _ := checkManifest(ctx, env)
		if r.Status != Failed || cfg != nil {
			t.Fatalf("unexpected result: %+v", r)
		}
	})
}

func TestCheckCredential(t *testing.T) {
	root := t.TempDir()
	t.Run("resolves", func(t *testing.T) {
		t.Setenv("MG_TEST_CRED", "sk-super-secret-value")
		r := checkCredential("agents.worker", root, workflow.Harness{Kind: "claude", Credential: "env:MG_TEST_CRED"})
		if r.Status != OK {
			t.Fatalf("unexpected result: %+v", r)
		}
		if strings.Contains(r.Detail, "sk-super-secret-value") || strings.Contains(r.Fix, "sk-super-secret-value") {
			t.Fatal("credential value leaked into a Result field")
		}
	})
	t.Run("missing", func(t *testing.T) {
		t.Setenv("MG_MISSING_CRED", "")
		r := checkCredential("agents.worker", root, workflow.Harness{Kind: "claude", Credential: ""})
		if r.Status != Failed || !strings.Contains(r.Fix, "ANTHROPIC_API_KEY") {
			t.Fatalf("unexpected result: %+v", r)
		}
	})
}

// Regression: agents.*.credential file: references must resolve against the
// workflow root (Config.Dir), not the possibly-narrowed invocation directory
// (Env.Dir, e.g. under `envctl -C <subdir> doctor`).
func TestReportResolvesFileCredentialAgainstWorktreeRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "claude-cred.json"), []byte(`{"ANTHROPIC_API_KEY":"sk-from-file"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeV2Manifest(t, root, "file:claude-cred.json", "file:claude-cred.json")
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	stubs := allOKStubs(root)
	env := Env{Run: stubRunner(t, stubs), Dir: sub, GOOS: "linux"}
	results, failed := Report(context.Background(), env)
	if failed {
		t.Fatalf("expected success, got failures: %+v", results)
	}
	for _, r := range results {
		if (r.Name == "agents.worker" || r.Name == "agents.supervisor") && r.Status != OK {
			t.Fatalf("credential resolution against Config.Dir failed: %+v", r)
		}
		if strings.Contains(r.Detail, "sk-from-file") || strings.Contains(r.Fix, "sk-from-file") {
			t.Fatal("credential value leaked into a Result field")
		}
	}
}

// allOKStubs returns a Runner stub set where every non-manifest check
// succeeds and the git toplevel resolves to root regardless of the
// requested -C directory (matching real git's behavior from any subdirectory).
func allOKStubs(root string) map[string]stub {
	return map[string]stub{
		cmdKey("docker", "info", "--format", "{{.OperatingSystem}}"):                    {code: 0},
		cmdKey("docker", "compose", "version", "--short"):                               {code: 0, stdout: "2.29.0\n"},
		cmdKey("git", "--version"):                                                      {code: 0, stdout: "git version 2.43.0\n"},
		cmdKey("limactl", "--version"):                                                  {code: 0, stdout: "limactl version 2.1.0\n"},
		cmdKey("gh", "auth", "status"):                                                  {code: 0},
		cmdKey("git", "-C", root, "rev-parse", "--show-toplevel"):                       {code: 0, stdout: root + "\n"},
		cmdKey("git", "-C", filepath.Join(root, "sub"), "rev-parse", "--show-toplevel"): {code: 0, stdout: root + "\n"},
	}
}

func TestCheckXcodeCLT(t *testing.T) {
	ctx := context.Background()

	t.Run("non-macOS is omitted", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{}), GOOS: "linux"}
		if r := checkXcodeCLT(ctx, env); r != nil {
			t.Fatalf("expected no results on linux, got %+v", r)
		}
	})

	t.Run("not installed", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{
			cmdKey("xcode-select", "-p"): {code: 2, stderr: "unable to get active developer directory"},
		}), GOOS: "darwin"}
		r := checkXcodeCLT(ctx, env)
		if len(r) != 1 || r[0].Status != Failed {
			t.Fatalf("unexpected result: %+v", r)
		}
	})

	t.Run("up to date", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{
			cmdKey("xcode-select", "-p"):                                      {code: 0, stdout: "/Library/Developer/CommandLineTools\n"},
			cmdKey("pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables"): {code: 0, stdout: "package-id: com.apple.pkg.CLTools_Executables\nversion: 16.2.0.0.1234567890\nvolume: /\n"},
			cmdKey("sw_vers", "-productVersion"):                              {code: 0, stdout: "16.5\n"},
		}), GOOS: "darwin"}
		r := checkXcodeCLT(ctx, env)
		if len(r) != 1 || r[0].Status != OK {
			t.Fatalf("unexpected result: %+v", r)
		}
	})

	// The exact regression reported on a real machine: xcode-select reports
	// the tools installed, but their major version trails the running
	// macOS's, which Homebrew refuses.
	t.Run("outdated for Homebrew", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{
			cmdKey("xcode-select", "-p"):                                      {code: 0, stdout: "/Library/Developer/CommandLineTools\n"},
			cmdKey("pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables"): {code: 0, stdout: "version: 16.2.0.0.1234567890\n"},
			cmdKey("sw_vers", "-productVersion"):                              {code: 0, stdout: "27.0\n"},
		}), GOOS: "darwin"}
		r := checkXcodeCLT(ctx, env)
		if len(r) != 1 || r[0].Status != Failed || !strings.Contains(r[0].Fix, "rm -rf /Library/Developer/CommandLineTools") {
			t.Fatalf("unexpected result: %+v", r)
		}
	})

	t.Run("undeterminable version is a warning", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{
			cmdKey("xcode-select", "-p"):                                      {code: 0, stdout: "/Library/Developer/CommandLineTools\n"},
			cmdKey("pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables"): {code: 1, stderr: "No receipt for 'com.apple.pkg.CLTools_Executables' found"},
			cmdKey("defaults", "read", "/Library/Developer/CommandLineTools/version.plist", "CFBundleShortVersionString"): {code: 1},
			cmdKey("sw_vers", "-productVersion"): {code: 0, stdout: "16.5\n"},
		}), GOOS: "darwin"}
		r := checkXcodeCLT(ctx, env)
		if len(r) != 1 || r[0].Status != Warning {
			t.Fatalf("unexpected result: %+v", r)
		}
	})

	t.Run("falls back to CommandLineTools directory version", func(t *testing.T) {
		env := Env{Run: stubRunner(t, map[string]stub{
			cmdKey("xcode-select", "-p"):                                      {code: 0, stdout: "/Library/Developer/CommandLineTools\n"},
			cmdKey("pkgutil", "--pkg-info=com.apple.pkg.CLTools_Executables"): {code: 1},
			cmdKey("defaults", "read", "/Library/Developer/CommandLineTools/version.plist", "CFBundleShortVersionString"): {code: 0, stdout: "16.2\n"},
			cmdKey("sw_vers", "-productVersion"): {code: 0, stdout: "16.5\n"},
		}), GOOS: "darwin"}
		r := checkXcodeCLT(ctx, env)
		if len(r) != 1 || r[0].Status != OK {
			t.Fatalf("unexpected result: %+v", r)
		}
	})
}
