package review

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "core.hooksPath=/dev/null"}, args...)...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	raw, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", args[0], err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func comparisonFixture(t *testing.T) (*runstore.Store, *workflow.Run, string, string) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	if err := os.Mkdir(source, 0700); err != nil {
		t.Fatal(err)
	}
	localGit(t, source, "init", "--quiet", "--template=")
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(source, name), []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("math.py", "def add(a, b):\n    return a - b\n")
	write(".gitattributes", "*.py diff=hostile\n")
	localGit(t, source, "add", ".")
	localGit(t, source, "commit", "--quiet", "-m", "base")
	base := localGit(t, source, "rev-parse", "HEAD")
	write("math.py", "def add(a, b):\n    return a + b\n")
	localGit(t, source, "commit", "--quiet", "-am", "fix addition")
	fixed := localGit(t, source, "rev-parse", "HEAD")
	file := filepath.Join(root, "source.bundle")
	localGit(t, source, "bundle", "create", file, "HEAD")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	store, err := runstore.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	bundle, err := store.PutArtifact("source", "application/x-git-bundle", raw)
	if err != nil {
		t.Fatal(err)
	}
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /now-absent}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("addition", "correct addition", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.Current().SourcePins = map[string]string{"app": base}
	old := workflow.Checkpoint{ID: "cp_old", Revision: run.CurrentRevision, Node: "code", HistoricalOnly: true, Result: workflow.Result{Summary: "original implementation", Commits: map[string]string{"app": base}, Sources: map[string]workflow.Artifact{"app": bundle}}}
	run.Current().Checkpoints["code"] = old
	if _, err = run.Rewind("code", "fix arithmetic", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	run.Current().SourcePins = map[string]string{"app": base}
	run.Current().Checkpoints["code"] = workflow.Checkpoint{ID: "cp_new", Revision: run.CurrentRevision, Node: "code", Result: workflow.Result{Summary: "correct implementation", Commits: map[string]string{"app": fixed}, Sources: map[string]workflow.Artifact{"app": bundle}}}
	if _, err = store.Create(context.Background(), "fixture", nil, run); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(source); err != nil {
		t.Fatal(err)
	}
	return store, run, base, fixed
}

func TestRetainedComparisonAcrossRevisionsWithoutSourceOrVM(t *testing.T) {
	s, r, base, fixed := comparisonFixture(t)
	marker := filepath.Join(t.TempDir(), "executed")
	script := filepath.Join(t.TempDir(), "diff.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ntouch \""+marker+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GIT_EXTERNAL_DIFF", script)
	t.Setenv("GIT_DIR", "/must-not-use-host-repository")
	for _, from := range []string{"", "cp_old"} {
		c, err := Compare(context.Background(), s, r.ID, Request{Node: "code", From: from})
		if err != nil || len(c.Repositories) != 1 {
			t.Fatal("compare failed", err)
		}
		d := c.Repositories[0]
		if d.Before != base || d.After != fixed || d.Unavailable != "" || !strings.Contains(d.Patch, "-    return a - b") || !strings.Contains(d.Patch, "+    return a + b") {
			t.Fatal("wrong source diff", d)
		}
		if from != "" && (c.From.Checkpoint == nil || !c.From.Checkpoint.HistoricalOnly || c.From.Objective == c.To.Objective) {
			t.Fatal("lost revision history metadata")
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("external diff executed")
	}
	current, err := s.Get(context.Background(), r.ID)
	if err != nil || current.Version != r.Version || workflow.Digest(current) != workflow.Digest(r) {
		t.Fatal("review mutated workflow", err)
	}
	if _, err := Compare(context.Background(), s, r.ID, Request{Node: "code", From: "another-run-checkpoint"}); err == nil {
		t.Fatal("accepted unrelated comparison checkpoint")
	}
}

func TestMissingRetainedSourceIsVisibleAndMetadataRemainsReviewable(t *testing.T) {
	s, r, _, _ := comparisonFixture(t)
	a := r.Current().Checkpoints["code"].Result.Sources["app"]
	if err := os.WriteFile(filepath.Join(s.Dir, "artifacts", a.Digest[:2], a.Digest), []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Compare(context.Background(), s, r.ID, Request{Node: "code"})
	if err != nil || c.Repositories[0].Unavailable == "" || c.Repositories[0].Patch != "" || c.To.Checkpoint.Result.Summary != "correct implementation" {
		t.Fatal("missing source hidden or metadata lost", err)
	}
}

func TestDiffOutputBoundAndTerminalRendering(t *testing.T) {
	b := limitedOutput{limit: 8}
	if n, err := b.Write([]byte("0123456789")); n != 10 || err != nil || !b.truncated || b.buf.Len() != 8 {
		t.Fatal("output bound lost")
	}
	c := Comparison{Repositories: []RepositoryDiff{{Repository: "app", Before: "a", After: "b", Patch: "+hello\x1b]52;c;clipboard\a\n", Truncated: true}}}
	text := Text(c)
	if strings.Contains(text, "\x1b") || strings.Contains(text, "clipboard") || !strings.Contains(text, "truncated") {
		t.Fatal("unsafe or unmarked review output")
	}
}
