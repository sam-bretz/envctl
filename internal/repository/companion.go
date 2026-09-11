package repository

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
)

// CompanionLimit bounds a source object companion and its payloads, matching
// the guest capture program (objects.py).
const CompanionLimit = 64 << 20

var payloadName = regexp.MustCompile(`^payload/[a-f0-9]{64}$`)
var payloadDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

type CompanionLFS struct {
	Path string
	OID  string
	Size int64
}

// CompanionRepository is one repository in a checkpoint's submodule tree. The
// root has Path "." and no Bundle; every submodule carries a Git bundle of its
// exact commit. LFS lists the objects committed in that repository only.
type CompanionRepository struct {
	Path   string
	Commit string
	Bundle string
	LFS    []CompanionLFS
}

// Companion is a verified source object companion bound to one root commit.
type Companion struct {
	Commit       string
	Repositories []CompanionRepository
	payloads     map[string][]byte
}

// Payload returns verified bytes by SHA-256 digest (a bundle or LFS object).
func (c *Companion) Payload(digest string) ([]byte, bool) {
	value, ok := c.payloads[digest]
	return value, ok
}

// ReadCompanion validates a companion as strictly as the guest's read_archive:
// bounded size, regular tar members, per-payload checksums, exact manifest
// fields, binding to pin, and no missing or unreferenced payloads.
func ReadCompanion(raw []byte, pin string) (*Companion, error) {
	if len(raw) > CompanionLimit {
		return nil, errors.New("source objects exceed 64 MiB")
	}
	values := map[string][]byte{}
	var total int64
	archive := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errors.New("invalid source object archive")
		}
		if header.Typeflag != tar.TypeReg || values[header.Name] != nil || (header.Name != "manifest.json" && !payloadName.MatchString(header.Name)) {
			return nil, errors.New("invalid source object archive member")
		}
		total += header.Size
		if header.Size < 0 || total > CompanionLimit {
			return nil, errors.New("source objects exceed 64 MiB")
		}
		value := make([]byte, header.Size)
		if _, err = io.ReadFull(archive, value); err != nil {
			return nil, errors.New("truncated source object")
		}
		if header.Name != "manifest.json" {
			sum := sha256.Sum256(value)
			if hex.EncodeToString(sum[:]) != header.Name[len("payload/"):] {
				return nil, errors.New("source object checksum mismatch")
			}
		}
		values[header.Name] = value
	}
	manifest, ok := values["manifest.json"]
	if !ok {
		return nil, errors.New("source object archive has no manifest")
	}
	delete(values, "manifest.json")
	var doc map[string]json.RawMessage
	if strictJSON(manifest, &doc) != nil || !exactKeys(doc, "version", "commit", "repositories") || string(doc["version"]) != "1" {
		return nil, errors.New("source object checkpoint binding differs")
	}
	var commit string
	if json.Unmarshal(doc["commit"], &commit) != nil || commit != pin {
		return nil, errors.New("source object checkpoint binding differs")
	}
	var entries []map[string]json.RawMessage
	if strictJSON(doc["repositories"], &entries) != nil || len(entries) == 0 || len(entries) > 256 {
		return nil, errors.New("invalid source object inventory")
	}
	c := &Companion{Commit: pin, payloads: map[string][]byte{}}
	paths := map[string]bool{}
	required := map[string]bool{}
	for _, entry := range entries {
		if entry == nil {
			return nil, errors.New("invalid source repository entry")
		}
		var repo CompanionRepository
		if json.Unmarshal(entry["path"], &repo.Path) != nil || !companionPath(repo.Path, true) {
			return nil, errors.New("invalid source object path")
		}
		root := repo.Path == "."
		if (root && !exactKeys(entry, "path", "commit", "lfs")) || (!root && !exactKeys(entry, "path", "commit", "lfs", "bundle")) {
			return nil, errors.New("invalid source repository fields")
		}
		if paths[repo.Path] || json.Unmarshal(entry["commit"], &repo.Commit) != nil || !sha.MatchString(repo.Commit) {
			return nil, errors.New("invalid source repository entry")
		}
		paths[repo.Path] = true
		if root {
			if repo.Commit != pin {
				return nil, errors.New("root source binding differs")
			}
		} else {
			if json.Unmarshal(entry["bundle"], &repo.Bundle) != nil || !payloadDigest.MatchString(repo.Bundle) {
				return nil, errors.New("invalid submodule bundle reference")
			}
			required["payload/"+repo.Bundle] = true
		}
		var objects []map[string]json.RawMessage
		if strictJSON(entry["lfs"], &objects) != nil || objects == nil {
			return nil, errors.New("invalid LFS inventory")
		}
		files := map[string]bool{}
		for _, object := range objects {
			var f CompanionLFS
			if object == nil || !exactKeys(object, "path", "oid", "size") {
				return nil, errors.New("invalid LFS entry")
			}
			size, err := strconv.ParseInt(string(object["size"]), 10, 64)
			if json.Unmarshal(object["path"], &f.Path) != nil || !companionPath(f.Path, false) || files[f.Path] || json.Unmarshal(object["oid"], &f.OID) != nil || !payloadDigest.MatchString(f.OID) || err != nil || size < 0 {
				return nil, errors.New("invalid LFS inventory")
			}
			f.Size = size
			files[f.Path] = true
			required["payload/"+f.OID] = true
			if int64(len(values["payload/"+f.OID])) != f.Size {
				return nil, errors.New("LFS size mismatch")
			}
			repo.LFS = append(repo.LFS, f)
		}
		c.Repositories = append(c.Repositories, repo)
	}
	if !paths["."] || len(required) != len(values) {
		return nil, errors.New("source archive has missing or unreferenced payloads")
	}
	for name, value := range values {
		if !required[name] {
			return nil, errors.New("source archive has missing or unreferenced payloads")
		}
		c.payloads[name[len("payload/"):]] = value
	}
	return c, nil
}

func strictJSON(raw []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	if err := d.Decode(out); err != nil {
		return err
	}
	if d.More() {
		return errors.New("trailing JSON")
	}
	return nil
}

func exactKeys(doc map[string]json.RawMessage, keys ...string) bool {
	if len(doc) != len(keys) {
		return false
	}
	for _, key := range keys {
		if _, ok := doc[key]; !ok {
			return false
		}
	}
	return true
}

// companionPath mirrors objects.py relative(): a normalized, relative POSIX
// path without "..", ".git" or empty components; "." only for the root.
func companionPath(value string, allowRoot bool) bool {
	if allowRoot && value == "." {
		return true
	}
	if value == "" || value == "." || strings.HasPrefix(value, "/") || strings.ContainsRune(value, 0) || path.Clean(value) != value {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == ".." || part == ".git" || part == "" {
			return false
		}
	}
	return true
}
