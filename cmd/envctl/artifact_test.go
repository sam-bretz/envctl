package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestArtifactExportNeverReplacesExistingContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "evidence.json")
	if err := exportArtifact(path, []byte("reviewed evidence")); err != nil {
		t.Fatal(err)
	}
	if err := exportArtifact(path, []byte("replacement")); err == nil {
		t.Fatal("existing output replaced")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if err := exportArtifact(link, []byte("replacement")); err == nil {
		t.Fatal("symlink destination accepted")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "reviewed evidence" {
		t.Fatal("evidence altered", err)
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 2 {
		t.Fatal("temporary output leaked", err)
	}
}
