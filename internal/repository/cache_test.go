package repository

import (
	"archive/tar"
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRetainedBaselinePreservesLegacyImportIdentity(t *testing.T) {
	ctx := context.Background()
	resolver := Resolver{Dir: t.TempDir()}
	source, err := resolver.Prepare(ctx, workflow.Repository{ID: "app", URL: sourceRepo(t), Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(resolver.Dir, "app.tar")
	if err = Archive(ctx, source, legacy); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatal(err)
	}
	// A harmless Git metadata update changes re-archiving bytes, but must not
	// change the archive identity already accepted by an existing guest.
	if err = os.WriteFile(filepath.Join(source.Checkout, ".git", "description"), []byte("updated coordinator metadata"), 0600); err != nil {
		t.Fatal(err)
	}
	_, archive, err := (Cache{Dir: t.TempDir()}).Prepare(ctx, resolver, source.Repository)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("migration changed accepted import bytes", err)
	}
}

func TestRetainedBaselineRestoresAfterOriginalAndPreparationDisappear(t *testing.T) {
	ctx := context.Background()
	origin := sourceRepo(t)
	cache := Cache{Dir: t.TempDir()}
	resolver := Resolver{Dir: t.TempDir()}
	ref := workflow.Repository{ID: "app", URL: origin, Ref: "main"}
	source, archive, err := cache.Prepare(ctx, resolver, ref)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(origin); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(resolver.Dir); err != nil {
		t.Fatal(err)
	}
	// Branch and publication metadata belong to the new revision; reusing
	// source must not restore a previous revision's output destination.
	ref = source.Repository
	ref.Branch = "revised-output"
	ref.ID = "renamed"
	restored, again, err := cache.Prepare(ctx, Resolver{Dir: filepath.Join(t.TempDir(), "unused")}, ref)
	if err != nil || again != archive || restored.Repository.ID != "renamed" || restored.Repository.Branch != "revised-output" {
		t.Fatal("retained baseline unavailable or metadata rebound", err)
	}
	file, err := os.Open(again)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := tar.NewReader(file)
	found := false
	for {
		h, e := reader.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		if h.Name == "file.txt" {
			raw, e := io.ReadAll(reader)
			if e != nil || string(raw) != "first" {
				t.Fatal("wrong retained source", e)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("archive lacks original source")
	}
}

func TestUnpinnedSourceStillResolvesCurrentRef(t *testing.T) {
	ctx := context.Background()
	origin := sourceRepo(t)
	cache := Cache{Dir: t.TempDir()}
	ref := workflow.Repository{ID: "app", URL: origin, Ref: "main"}
	first, _, err := cache.Prepare(ctx, Resolver{Dir: t.TempDir()}, ref)
	if err != nil {
		t.Fatal(err)
	}
	commit(t, origin, "second")
	second, _, err := cache.Prepare(ctx, Resolver{Dir: t.TempDir()}, ref)
	if err != nil {
		t.Fatal(err)
	}
	if first.Repository.BaseSHA == second.Repository.BaseSHA {
		t.Fatal("cache changed meaning of an unpinned ref")
	}
}

func TestCorruptRetainedBaselineDoesNotFallbackSilently(t *testing.T) {
	ctx := context.Background()
	origin := sourceRepo(t)
	cache := Cache{Dir: t.TempDir()}
	source, archive, err := cache.Prepare(ctx, Resolver{Dir: t.TempDir()}, workflow.Repository{ID: "app", URL: origin, Ref: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(archive, []byte("corrupt"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = cache.Prepare(ctx, Resolver{Dir: t.TempDir()}, source.Repository); err == nil {
		t.Fatal("corrupt archive accepted or silently replaced")
	}
	if err = os.Remove(archive); err != nil {
		t.Fatal(err)
	}
	if _, _, err = cache.Prepare(ctx, Resolver{Dir: t.TempDir()}, source.Repository); err == nil {
		t.Fatal("missing archive silently replaced")
	}
}
