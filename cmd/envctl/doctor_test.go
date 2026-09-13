package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/doctor"
)

func writeFakeBin(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// doctorFixture writes a real worktree (envctl.yaml + a file: credential) and
// a PATH of tiny fake docker/git/limactl/gh binaries, exercising doctorCmd
// end to end through the real exec-based Runner with no actual tool
// dependency. dockerOK controls whether the fake docker binary succeeds, so
// callers can force a Failed check.
func doctorFixture(t *testing.T, dockerOK bool) (worktree string) {
	t.Helper()
	worktree = t.TempDir()
	if err := os.WriteFile(filepath.Join(worktree, "claude-cred.json"), []byte(`{"ANTHROPIC_API_KEY":"sk-doctor-test-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := "version: 2\nproject: mg\nrepositories:\n  - id: main\n    url: https://example.com/repo.git\nworkflow:\n  template: feature\nagents:\n  worker:\n    kind: claude\n    credential: file:claude-cred.json\n  supervisor:\n    kind: claude\n    credential: file:claude-cred.json\n"
	if err := os.WriteFile(filepath.Join(worktree, "envctl.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	bin := t.TempDir()
	dockerExit := "exit 0"
	if !dockerOK {
		dockerExit = "exit 1"
	}
	writeFakeBin(t, bin, "docker", `
case "$*" in
  "info --format {{.OperatingSystem}}") `+dockerExit+` ;;
  "compose version --short") echo "v2.29.0"; exit 0 ;;
esac
exit 1
`)
	writeFakeBin(t, bin, "git", `
case "$*" in
  "--version") echo "git version 2.43.0"; exit 0 ;;
  *"rev-parse --show-toplevel") echo "`+worktree+`"; exit 0 ;;
esac
exit 1
`)
	writeFakeBin(t, bin, "limactl", `echo "limactl version 2.1.0"`)
	writeFakeBin(t, bin, "gh", `exit 0`)
	t.Setenv("PATH", bin)

	// Platform-specific checks (Xcode CLT) must be deterministic on any test
	// host, not just the host actually running `go test`.
	saved := goos
	goos = "linux"
	t.Cleanup(func() { goos = saved })

	return worktree
}

func runDoctor(t *testing.T, worktree string, extraArgs ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	cmd := root()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"-C", worktree, "doctor"}, extraArgs...))
	err := cmd.Execute()
	return out.String(), err
}

func TestDoctorAllOK(t *testing.T) {
	worktree := doctorFixture(t, true)
	out, err := runDoctor(t, worktree)
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}
	if strings.Contains(out, "sk-doctor-test-secret") {
		t.Fatal("credential value leaked into human output")
	}
}

func TestDoctorAllOKJSON(t *testing.T) {
	worktree := doctorFixture(t, true)
	out, err := runDoctor(t, worktree, "--json")
	if err != nil {
		t.Fatalf("unexpected error: %v\n%s", err, out)
	}
	var results []doctor.Result
	if jsonErr := json.Unmarshal([]byte(out), &results); jsonErr != nil {
		t.Fatalf("invalid JSON: %v\n%s", jsonErr, out)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one check result")
	}
	for _, r := range results {
		if r.Name == "" || r.Status == "" || r.Detail == "" {
			t.Fatalf("incomplete result: %+v", r)
		}
	}
	if strings.Contains(out, "sk-doctor-test-secret") {
		t.Fatal("credential value leaked into JSON output")
	}
}

func TestDoctorFailureSetsExitSentinel(t *testing.T) {
	worktree := doctorFixture(t, false)
	out, err := runDoctor(t, worktree)
	if !errors.Is(err, errDoctorFailed) {
		t.Fatalf("expected errDoctorFailed, got %v\n%s", err, out)
	}
}
