// Package repository resolves source inputs before worker execution. Preparation
// uses independent clones, never a writable mount of the developer's checkout.
package repository

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

var sha = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var safeID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Source struct {
	Repository workflow.Repository `json:"repository"`
	Checkout   string              `json:"checkout"`
	Submodules map[string]string   `json:"submodules"`
	LFS        bool                `json:"lfs"`
}
type Resolver struct{ Dir string }

func git(ctx context.Context, dir string, args ...string) (string, error) {
	args = append([]string{"-c", "core.hooksPath=/dev/null"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_LFS_SKIP_SMUDGE=1")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	b, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[2], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(b)), nil
}
func validateURL(raw string) error {
	if strings.ContainsAny(raw, "\x00\r\n") {
		return errors.New("invalid repository URL")
	}
	u, err := url.Parse(raw)
	if err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.User != nil {
		return errors.New("repository URLs must not embed credentials; use a credential helper")
	}
	return nil
}

// Prepare produces an independent checkout at an exact SHA. Subsequent calls
// with BaseSHA set keep that SHA even when its source branch moves.
func (r Resolver) Prepare(ctx context.Context, repo workflow.Repository) (Source, error) {
	if !safeID.MatchString(repo.ID) {
		return Source{}, errors.New("invalid repository ID")
	}
	if err := validateURL(repo.URL); err != nil {
		return Source{}, err
	}
	if err := os.MkdirAll(r.Dir, 0700); err != nil {
		return Source{}, err
	}
	final := filepath.Join(r.Dir, repo.ID)
	manifest := filepath.Join(r.Dir, "."+repo.ID+".source.json")
	if data, err := os.ReadFile(manifest); err == nil {
		var previous Source
		if err = json.Unmarshal(data, &previous); err != nil {
			return Source{}, err
		}
		if previous.Repository.URL != repo.URL || previous.Repository.Ref != repo.Ref || (repo.BaseSHA != "" && repo.BaseSHA != previous.Repository.BaseSHA) {
			return Source{}, errors.New("repository preparation ID already belongs to different inputs")
		}
		repo.BaseSHA = previous.Repository.BaseSHA
		if _, err = os.Stat(final); err == nil {
			head, e := git(ctx, final, "rev-parse", "HEAD")
			if e != nil || head != repo.BaseSHA {
				return Source{}, errors.New("prepared checkout no longer matches its recorded pin")
			}
			dirty, e := git(ctx, final, "status", "--porcelain")
			if e != nil || dirty != "" {
				return Source{}, errors.New("prepared checkout has unrecorded changes")
			}
			return previous, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return Source{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Source{}, err
	}
	if _, err := os.Stat(final); err == nil {
		return Source{}, errors.New("unrecorded checkout exists; refusing to overwrite it")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Source{}, err
	}
	staging, err := os.MkdirTemp(r.Dir, "."+repo.ID+"-preparing-")
	if err != nil {
		return Source{}, err
	}
	defer os.RemoveAll(staging)
	checkout := filepath.Join(staging, "checkout")
	// --no-local prevents hardlinks and alternates into the source's object store.
	if _, err := git(ctx, r.Dir, "clone", "--no-local", "--no-checkout", "--", repo.URL, checkout); err != nil {
		return Source{}, err
	}
	ref := repo.Ref
	if repo.BaseSHA != "" {
		if !sha.MatchString(repo.BaseSHA) {
			return Source{}, errors.New("invalid source pin")
		}
		ref = repo.BaseSHA
	}
	if ref == "" {
		ref = "HEAD"
	}
	resolved, err := git(ctx, checkout, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil {
		return Source{}, err
	}
	if !sha.MatchString(resolved) {
		return Source{}, errors.New("source did not resolve to immutable commit")
	}
	repo.BaseSHA = resolved
	if _, err = git(ctx, checkout, "checkout", "--detach", resolved); err != nil {
		return Source{}, err
	}
	// Git itself validates submodule paths and resolves relative URLs against the
	// origin. Do not enable arbitrary file transport from untrusted .gitmodules.
	submodules := map[string]string{}
	if _, err = os.Stat(filepath.Join(checkout, ".gitmodules")); err == nil {
		if _, err = git(ctx, checkout, "submodule", "update", "--init", "--recursive"); err != nil {
			return Source{}, err
		}
		out, err := git(ctx, checkout, "submodule", "foreach", "--quiet", "--recursive", `printf '%s\0%s\0' "$(git rev-parse HEAD)" "$displaypath"`)
		if err != nil {
			return Source{}, err
		}
		fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
		if out != "" {
			if len(fields)%2 != 0 {
				return Source{}, errors.New("invalid recursive submodule inventory")
			}
			for i := 0; i < len(fields); i += 2 {
				if !sha.MatchString(fields[i]) || fields[i+1] == "" {
					return Source{}, errors.New("unresolved submodule")
				}
				submodules[fields[i+1]] = fields[i]
			}
		}
	}
	lfs := false
	checkouts := map[string]string{checkout: resolved}
	for name, pin := range submodules {
		checkouts[filepath.Join(checkout, filepath.FromSlash(name))] = pin
	}
	// Nested repositories own separate LFS object stores. Fetch and hydrate each
	// before archiving; a populated main repository cannot prove its modules.
	for directory, pin := range checkouts {
		attrs, e := git(ctx, directory, "grep", "-l", "filter=lfs", pin, "--", ".gitattributes", "**/.gitattributes")
		if e == nil && attrs != "" {
			lfs = true
			if _, err = git(ctx, directory, "-c", "lfs.fetchinclude=", "-c", "lfs.fetchexclude=", "lfs", "fetch", "origin", pin); err != nil {
				return Source{}, err
			}
			if _, err = git(ctx, directory, "-c", "lfs.fetchinclude=", "-c", "lfs.fetchexclude=", "lfs", "checkout"); err != nil {
				return Source{}, err
			}
			if _, err = git(ctx, directory, "lfs", "fsck"); err != nil {
				return Source{}, err
			}
		}
	}
	source := Source{Repository: repo, Checkout: final, Submodules: submodules, LFS: lfs}
	data, err := json.Marshal(source)
	if err != nil {
		return Source{}, err
	}
	metadata := filepath.Join(staging, "source.json")
	if err = os.WriteFile(metadata, data, 0600); err != nil {
		return Source{}, err
	}
	if err = os.Rename(metadata, manifest); err != nil {
		return Source{}, err
	}
	if err = os.Rename(checkout, final); err != nil {
		return Source{}, err
	}
	return source, nil
}

// Archive preserves Git metadata and submodule object stores so guest workers
// can commit and use independent worktrees after source transfer.
func Archive(ctx context.Context, source Source, destination string) error {
	if !sha.MatchString(source.Repository.BaseSHA) {
		return errors.New("unresolved source cannot be archived")
	}
	file, err := os.CreateTemp(filepath.Dir(destination), ".source-archive-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	defer file.Close()
	writer := tar.NewWriter(file)
	err = filepath.WalkDir(source.Checkout, func(filename string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, err := filepath.Rel(source.Checkout, filename)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() && !info.IsDir() && info.Mode()&os.ModeSymlink == 0 {
			return errors.New("source archive contains an unsupported special file")
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, err = os.Readlink(filename)
			if err != nil {
				return err
			}
		}
		header, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		// Archive only repository content and portable modes. macOS copyfile
		// metadata would create ._pack-*.idx files that corrupt Git's pack scan.
		header.Uid, header.Gid = 0, 0
		header.Uname, header.Gname = "", ""
		header.ModTime = time.Unix(0, 0)
		header.AccessTime = time.Time{}
		header.ChangeTime = time.Time{}
		header.Xattrs = nil
		header.PAXRecords = nil
		if err = writer.WriteHeader(header); err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		input, err := os.Open(filename)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(writer, archiveReader{ctx: ctx, reader: input})
		closeErr := input.Close()
		if err = errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if n != info.Size() {
			return errors.New("source changed while archiving")
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = writer.Close(); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(file.Name(), destination)
}

type archiveReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r archiveReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
