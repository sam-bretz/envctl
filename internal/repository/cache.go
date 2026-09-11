package repository

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// Cache retains complete baseline archives outside disposable revision VMs.
// Checkpoint bundles supply later commits; this archive also preserves baseline
// submodule object stores and fetched LFS objects. Cache hits require an exact
// source pin and never resolve a moving ref to an older cached version.
type Cache struct{ Dir string }

type retainedSource struct {
	URL        string            `json:"url"`
	Pin        string            `json:"pin"`
	Submodules map[string]string `json:"submodules"`
	LFS        bool              `json:"lfs"`
	Archive    workflow.Artifact `json:"archive"`
}

func (c Cache) index(repo workflow.Repository) string {
	key := workflow.Digest(struct{ URL, Pin string }{repo.URL, repo.BaseSHA})
	return filepath.Join(c.Dir, "bindings", key+".json")
}

func (c Cache) read(ctx context.Context, repo workflow.Repository) (Source, string, error) {
	raw, err := os.ReadFile(c.index(repo))
	if err != nil {
		return Source{}, "", err
	}
	var record retainedSource
	if json.Unmarshal(raw, &record) != nil || record.URL != repo.URL || record.Pin != repo.BaseSHA || !sha.MatchString(record.Pin) || record.Archive.MediaType != "application/x-tar" || record.Archive.Size < 1 {
		return Source{}, "", errors.New("retained baseline identity is invalid")
	}
	digest, err := hex.DecodeString(record.Archive.Digest)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != record.Archive.Digest {
		return Source{}, "", errors.New("retained baseline digest is invalid")
	}
	archive := filepath.Join(c.Dir, "archives", record.Archive.Digest+".tar")
	actual, size, err := archiveDigest(ctx, archive)
	if err != nil || actual != record.Archive.Digest || size != record.Archive.Size {
		return Source{}, "", errors.New("retained baseline archive is missing or corrupt")
	}
	return Source{Repository: repo, Submodules: record.Submodules, LFS: record.LFS}, archive, nil
}

func (c Cache) Prepare(ctx context.Context, resolver Resolver, repo workflow.Repository) (Source, string, error) {
	if !safeID.MatchString(repo.ID) {
		return Source{}, "", errors.New("invalid repository ID")
	}
	if err := validateURL(repo.URL); err != nil {
		return Source{}, "", err
	}
	if repo.BaseSHA != "" {
		if !sha.MatchString(repo.BaseSHA) {
			return Source{}, "", errors.New("invalid retained baseline pin")
		}
		if source, archive, err := c.read(ctx, repo); err == nil || !os.IsNotExist(err) {
			return source, archive, err
		}
	}
	source, err := resolver.Prepare(ctx, repo)
	if err != nil {
		return Source{}, "", err
	}
	repo = source.Repository
	// Another invocation may have retained this exact resolved input while its
	// moving source ref was being resolved. Reuse the verified winner.
	if cached, archive, err := c.read(ctx, repo); err == nil || !os.IsNotExist(err) {
		return cached, archive, err
	}
	if err = os.MkdirAll(c.Dir, 0700); err != nil {
		return Source{}, "", err
	}
	stage, err := os.MkdirTemp(c.Dir, ".retaining-")
	if err != nil {
		return Source{}, "", err
	}
	defer os.RemoveAll(stage)
	archive := filepath.Join(stage, "source.tar")
	// Earlier backends retained a per-preparation archive. Preserve those exact
	// bytes during migration: a guest may already have accepted their digest
	// before its coordinator lost the preparation acknowledgement.
	legacy, e := os.Open(filepath.Join(resolver.Dir, repo.ID+".tar"))
	if e == nil {
		file, e := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			legacy.Close()
			return Source{}, "", e
		}
		_, e = io.Copy(file, archiveReader{ctx: ctx, reader: legacy})
		e = errors.Join(e, legacy.Close(), file.Sync(), file.Close())
		if e != nil {
			return Source{}, "", e
		}
	} else if !os.IsNotExist(e) {
		return Source{}, "", e
	} else if err = Archive(ctx, source, archive); err != nil {
		return Source{}, "", err
	}
	digest, size, err := archiveDigest(ctx, archive)
	if err != nil {
		return Source{}, "", err
	}
	if err = os.MkdirAll(filepath.Join(c.Dir, "archives"), 0700); err != nil {
		return Source{}, "", err
	}
	destination := filepath.Join(c.Dir, "archives", digest+".tar")
	if err = os.Link(archive, destination); err != nil && !os.IsExist(err) {
		return Source{}, "", err
	}
	if err = syncDirectory(filepath.Dir(destination)); err != nil {
		return Source{}, "", err
	}
	record := retainedSource{URL: repo.URL, Pin: repo.BaseSHA, Submodules: source.Submodules, LFS: source.LFS, Archive: workflow.Artifact{Name: "baseline", MediaType: "application/x-tar", Digest: digest, Size: size}}
	raw, err := json.Marshal(record)
	if err != nil {
		return Source{}, "", err
	}
	index := c.index(repo)
	if err = os.MkdirAll(filepath.Dir(index), 0700); err != nil {
		return Source{}, "", err
	}
	file, err := os.OpenFile(filepath.Join(stage, "record.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return Source{}, "", err
	}
	_, err = file.Write(raw)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return Source{}, "", err
	}
	if err = os.Link(file.Name(), index); err != nil && !os.IsExist(err) {
		return Source{}, "", err
	}
	if err = syncDirectory(filepath.Dir(index)); err != nil {
		return Source{}, "", err
	}
	return c.read(ctx, repo)
}

func archiveDigest(ctx context.Context, filename string) (string, int64, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	size, err := io.Copy(h, archiveReader{ctx: ctx, reader: f})
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), size, nil
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
