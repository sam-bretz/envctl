package repository

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerifyMergeRetainedHistoryAndConflictResolution(t *testing.T) {
	root := sourceRepo(t)
	base := objectCommand(t, root, "rev-parse", "HEAD")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, "conflict.txt"), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		objectCommand(t, root, "add", "conflict.txt")
		objectCommand(t, root, "commit", "-qm", body)
	}
	write("left\n")
	left := objectCommand(t, root, "rev-parse", "HEAD")
	objectCommand(t, root, "checkout", "--detach", base)
	write("right\n")
	right := objectCommand(t, root, "rev-parse", "HEAD")
	bundle := func() []byte {
		t.Helper()
		file := filepath.Join(t.TempDir(), "source.bundle")
		objectCommand(t, root, "bundle", "create", file, "HEAD")
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if err := VerifyMerge(context.Background(), bundle(), right, []string{left, right}); err == nil {
		t.Fatal("dropping the left input was accepted")
	}
	// This is an actual add/add conflict, resolved by work rather than choosing
	// whichever checkpoint appeared last in a Go map.
	if _, err := git(context.Background(), root, "merge", "--no-edit", left); err == nil {
		t.Fatal("fixture did not produce a conflict")
	}
	write("left and right\n")
	merged := objectCommand(t, root, "rev-parse", "HEAD")
	retained := bundle()
	if err := VerifyMerge(context.Background(), retained, merged, []string{left, right}); err != nil {
		t.Fatal("resolved merge rejected", err)
	}
	// Replacement refs in an untrusted bundle cannot manufacture ancestry.
	objectCommand(t, root, "replace", right, merged)
	poisoned := filepath.Join(t.TempDir(), "replaced.bundle")
	objectCommand(t, root, "--no-replace-objects", "bundle", "create", poisoned, "--all")
	poisonedBytes, err := os.ReadFile(poisoned)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMerge(context.Background(), poisonedBytes, right, []string{left}); err == nil {
		t.Fatal("replacement refs forged ancestry")
	}
	objectCommand(t, root, "replace", "-d", right)
	thin := filepath.Join(t.TempDir(), "thin.bundle")
	objectCommand(t, root, "bundle", "create", thin, "HEAD", "^"+left)
	thinBytes, err := os.ReadFile(thin)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMerge(context.Background(), thinBytes, merged, []string{left, right}); err == nil {
		t.Fatal("bundle needing unavailable prerequisites accepted")
	}
	// A hostile ambient Git directory/config must not affect the coordinator's
	// independent ancestry check or load replace refs from a worker checkout.
	t.Setenv("GIT_DIR", root+"/.git")
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.repositoryformatversion")
	t.Setenv("GIT_CONFIG_VALUE_0", "999")
	if err := VerifyMerge(context.Background(), retained, merged, []string{left, right}); err != nil {
		t.Fatal("ambient configuration affected verifier", err)
	}
	for _, parents := range [][]string{nil, {"--all"}, {strings.Repeat("f", 40)}} {
		if err := VerifyMerge(context.Background(), retained, merged, parents); err == nil {
			t.Fatal("invalid or absent input accepted", parents)
		}
	}
	if err := VerifyMerge(context.Background(), []byte("not a bundle"), merged, []string{left}); err == nil {
		t.Fatal("invalid bundle accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := VerifyMerge(cancelled, retained, merged, []string{left}); err == nil {
		t.Fatal("cancelled verification proceeded")
	}
}

func TestVerifyMergeSHA256Repository(t *testing.T) {
	root := t.TempDir()
	objectCommand(t, root, "init", "--object-format=sha256")
	objectCommand(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "--allow-empty", "-qm", "base")
	base := objectCommand(t, root, "rev-parse", "HEAD")
	objectCommand(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@localhost", "commit", "--allow-empty", "-qm", "fast-forward")
	head := objectCommand(t, root, "rev-parse", "HEAD")
	filename := filepath.Join(t.TempDir(), "sha256.bundle")
	objectCommand(t, root, "bundle", "create", filename, "HEAD")
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyMerge(context.Background(), raw, head, []string{base, head}); err != nil {
		t.Fatal(err)
	}
}
