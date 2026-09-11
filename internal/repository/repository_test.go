package repository

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestArchiveIsPortableAndDeterministic(t *testing.T) {
	ctx := context.Background()
	source, err := (Resolver{Dir: t.TempDir()}).Prepare(ctx, workflow.Repository{ID: "app", URL: sourceRepo(t), Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if binary, err := exec.LookPath("xattr"); err == nil {
		if b, err := exec.Command(binary, "-w", "user.envctl-fixture", "metadata-only", filepath.Join(source.Checkout, "file.txt")).CombinedOutput(); err != nil {
			t.Fatalf("set archive metadata fixture: %v %s", err, b)
		}
	}
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one.tar"), filepath.Join(dir, "two.tar")
	if err = Archive(ctx, source, one); err != nil {
		t.Fatal(err)
	}
	if err = os.Chtimes(filepath.Join(source.Checkout, "file.txt"), time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err = Archive(ctx, source, two); err != nil {
		t.Fatal(err)
	}
	a, err := os.ReadFile(one)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(two)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("filesystem timestamps changed source archive identity")
	}
	expected := map[string]bool{}
	if err = filepath.WalkDir(source.Checkout, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source.Checkout, p)
		if err != nil {
			return err
		}
		if rel != "." {
			expected[filepath.ToSlash(rel)] = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	reader := tar.NewReader(bytes.NewReader(a))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !expected[header.Name] {
			t.Fatalf("archive introduced non-repository metadata file %q", header.Name)
		}
		delete(expected, header.Name)
		if len(header.Xattrs) != 0 || header.Uid != 0 || header.Gid != 0 {
			t.Fatal("archive retained host identity or extended attributes")
		}
	}
	if len(expected) > 0 {
		t.Fatal("archive omitted source files", expected)
	}
}

func sourceRepo(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for _, args := range [][]string{{"init", "-b", "main"}, {"config", "user.email", "test@example.com"}, {"config", "user.name", "Test"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v", b, err)
		}
	}
	commit(t, d, "first")
	return d
}
func commit(t *testing.T, d, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(d, "file.txt"), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "file.txt"}, {"commit", "-m", text}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = d
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s %v", b, err)
		}
	}
}
func TestSourcePinsSurviveBranchMovement(t *testing.T) {
	ctx := context.Background()
	origin := sourceRepo(t)
	r := Resolver{Dir: t.TempDir()}
	a, err := r.Prepare(ctx, workflow.Repository{ID: "app", URL: origin, Ref: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	commit(t, origin, "second")
	next := Resolver{Dir: t.TempDir()}
	b, err := next.Prepare(ctx, a.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if b.Repository.BaseSHA != a.Repository.BaseSHA {
		t.Fatal("pin moved")
	}
	data, err := os.ReadFile(filepath.Join(b.Checkout, "file.txt"))
	if err != nil || string(data) != "first" {
		t.Fatal("wrong checkout", err)
	}
	if _, err = os.Stat(filepath.Join(b.Checkout, ".git", "objects", "info", "alternates")); !os.IsNotExist(err) {
		t.Fatal("source object store shared")
	}
	if err = os.WriteFile(filepath.Join(b.Checkout, "file.txt"), []byte("worker edit"), 0600); err != nil {
		t.Fatal(err)
	}
	host, _ := os.ReadFile(filepath.Join(origin, "file.txt"))
	if string(host) != "second" {
		t.Fatal("host source mutated")
	}
	archive := filepath.Join(t.TempDir(), "source.tar")
	if err = Archive(ctx, b, archive); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(archive); err != nil || st.Size() == 0 {
		t.Fatal("empty archive")
	}
}
func TestMultipleRepositoriesAndCredentialURLs(t *testing.T) {
	ctx := context.Background()
	origin := sourceRepo(t)
	r := Resolver{Dir: t.TempDir()}
	for _, id := range []string{"api", "web"} {
		if _, err := r.Prepare(ctx, workflow.Repository{ID: id, URL: origin, Ref: "main"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.Prepare(ctx, workflow.Repository{ID: "bad", URL: "https://secret@example.com/repo.git"}); err == nil {
		t.Fatal("credential URL recorded")
	}
}

func TestPreparationReceiptSurvivesRetryAndBranchMovement(t *testing.T) {
	ctx := context.Background()
	origin := sourceRepo(t)
	r := Resolver{Dir: t.TempDir()}
	spec := workflow.Repository{ID: "app", URL: origin, Ref: "main"}
	first, err := r.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, origin, "later")
	again, err := r.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if again.Repository.BaseSHA != first.Repository.BaseSHA {
		t.Fatal("retry re-resolved mutable branch")
	}
	// Simulate a crash after the pin receipt committed but before checkout rename.
	if err = os.RemoveAll(first.Checkout); err != nil {
		t.Fatal(err)
	}
	restored, err := r.Prepare(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Repository.BaseSHA != first.Repository.BaseSHA {
		t.Fatal("crash recovery lost source pin")
	}
}
