package repository

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func rewriteCompanion(t *testing.T, raw []byte, edit func(name string, value []byte) (string, []byte, bool), add ...[]byte) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	r := tar.NewReader(bytes.NewReader(raw))
	for {
		h, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		value, _ := io.ReadAll(r)
		name, value, keep := edit(h.Name, value)
		if !keep {
			continue
		}
		if err = w.WriteHeader(&tar.Header{Name: name, Mode: 0600, Size: int64(len(value)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write(value)
	}
	for _, value := range add {
		sum := sha256.Sum256(value)
		_ = w.WriteHeader(&tar.Header{Name: "payload/" + hex.EncodeToString(sum[:]), Mode: 0600, Size: int64(len(value)), Typeflag: tar.TypeReg})
		_, _ = w.Write(value)
	}
	_ = w.Close()
	return out.Bytes()
}

func TestReadCompanionAcceptsGuestCaptureAndRejectsTampering(t *testing.T) {
	root, _ := nestedObjectRepo(t)
	pin := objectCommand(t, root, "rev-parse", "HEAD")
	filename := filepath.Join(t.TempDir(), "objects.tar")
	objectsOK(t, root, pin, filename, "capture")
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	c, err := ReadCompanion(raw, pin)
	if err != nil {
		t.Fatal("real guest companion rejected:", err)
	}
	paths := map[string]CompanionRepository{}
	for _, repo := range c.Repositories {
		paths[repo.Path] = repo
	}
	middle, leaf := paths["vendor/middle module"], paths["vendor/middle module/leaf module"]
	if len(paths) != 3 || paths["."].Bundle != "" || len(paths["."].LFS) != 1 || middle.Bundle == "" || leaf.Bundle == "" || len(leaf.LFS) != 1 {
		t.Fatal("unexpected companion inventory", c.Repositories)
	}
	if middle.Commit != objectCommand(t, filepath.Join(root, "vendor/middle module"), "rev-parse", "HEAD") {
		t.Fatal("submodule commit not bound")
	}
	if body, ok := c.Payload(leaf.LFS[0].OID); !ok || string(body) != "leaf baseline\x00" {
		t.Fatal("LFS payload unavailable")
	}
	if _, err = ReadCompanion(raw, strings.Repeat("0", 40)); err == nil {
		t.Fatal("companion accepted for a different commit")
	}
	tampered := rewriteCompanion(t, raw, func(name string, value []byte) (string, []byte, bool) {
		if strings.HasPrefix(name, "payload/") && bytes.Contains(value, []byte("leaf baseline")) {
			return name, []byte("leaf forged!!\x00"), true
		}
		return name, value, true
	})
	same := func(name string, value []byte) (string, []byte, bool) { return name, value, true }
	if _, err = ReadCompanion(rewriteCompanion(t, raw, same), pin); err != nil {
		t.Fatal("rewrite helper altered a valid companion:", err)
	}
	extra := rewriteCompanion(t, raw, same, []byte("unreferenced"))
	missing := rewriteCompanion(t, raw, func(name string, value []byte) (string, []byte, bool) {
		return name, value, !(strings.HasPrefix(name, "payload/") && bytes.Contains(value, []byte("root baseline")))
	})
	manifest := rewriteCompanion(t, raw, func(name string, value []byte) (string, []byte, bool) {
		if name == "manifest.json" {
			return name, bytes.Replace(value, []byte(`"version":1`), []byte(`"version":true`), 1), true
		}
		return name, value, true
	})
	escape := rewriteCompanion(t, raw, func(name string, value []byte) (string, []byte, bool) {
		if name == "manifest.json" {
			return name, bytes.Replace(value, []byte(`"vendor/middle module"`), []byte(`"vendor/../middle module"`), 1), true
		}
		return name, value, true
	})
	for label, candidate := range map[string][]byte{"tampered payload": tampered, "unreferenced payload": extra, "missing payload": missing, "manifest type": manifest, "path escape": escape, "truncated": raw[:len(raw)/2], "oversized": make([]byte, CompanionLimit+1)} {
		if _, err := ReadCompanion(candidate, pin); err == nil {
			t.Fatal("accepted", label)
		}
	}
}
