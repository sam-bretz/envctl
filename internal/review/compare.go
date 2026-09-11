// Package review reconstructs read-only comparisons from retained checkpoints.
// It needs neither a running VM nor the original source checkout or remote.
package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type Request struct {
	Revision string `json:"revision"`
	Node     string `json:"node"`
	From     string `json:"from,omitempty"` // checkpoint ID; empty means target revision's source pins
}
type Endpoint struct {
	Revision     string               `json:"revision"`
	Objective    string               `json:"objective"`
	ConfigDigest string               `json:"config_digest"`
	Checkpoint   *workflow.Checkpoint `json:"checkpoint,omitempty"`
}
type RepositoryDiff struct {
	Repository  string `json:"repository"`
	Before      string `json:"before"`
	After       string `json:"after"`
	Patch       string `json:"patch"`
	Truncated   bool   `json:"truncated,omitempty"`
	Unavailable string `json:"unavailable,omitempty"`
}
type Comparison struct {
	Run          string           `json:"run"`
	From         Endpoint         `json:"from"`
	To           Endpoint         `json:"to"`
	Repositories []RepositoryDiff `json:"repositories"`
}

func Compare(ctx context.Context, store *runstore.Store, runID string, req Request) (Comparison, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	run, err := store.Get(ctx, runID)
	if err != nil {
		return Comparison{}, err
	}
	if req.Revision == "" {
		req.Revision = run.CurrentRevision
	}
	rev := run.Revision(req.Revision)
	if rev == nil {
		return Comparison{}, errors.New("review revision not found")
	}
	cp, ok := rev.Checkpoints[req.Node]
	if !ok {
		return Comparison{}, errors.New("review requires a recorded checkpoint")
	}
	out := Comparison{Run: runID, To: Endpoint{Revision: rev.ID, Objective: rev.Objective, ConfigDigest: cp.ConfigDigest, Checkpoint: &cp}, From: Endpoint{Revision: rev.ID, Objective: rev.Objective, ConfigDigest: workflow.Digest(rev.Config)}}
	before := rev.SourcePins
	if req.From != "" {
		found := false
		for _, r := range run.Revisions {
			for _, candidate := range r.Checkpoints {
				if candidate.ID == req.From && !found {
					out.From = Endpoint{Revision: r.ID, Objective: r.Objective, ConfigDigest: candidate.ConfigDigest, Checkpoint: &candidate}
					before = candidate.Result.Commits
					found = true
				}
			}
		}
		if !found {
			return Comparison{}, errors.New("comparison checkpoint is not in this run")
		}
	}
	ids := map[string]bool{}
	for id := range before {
		ids[id] = true
	}
	for id := range cp.Result.Commits {
		ids[id] = true
	}
	ordered := make([]string, 0, len(ids))
	for id := range ids {
		ordered = append(ordered, id)
	}
	sort.Strings(ordered)
	for _, id := range ordered {
		diff := RepositoryDiff{Repository: id, Before: before[id], After: cp.Result.Commits[id]}
		if diff.Before != diff.After {
			sources := []workflow.Artifact{cp.Result.Sources[id]}
			if out.From.Checkpoint != nil {
				sources = append(sources, out.From.Checkpoint.Result.Sources[id])
			}
			diff.Patch, diff.Truncated, err = sourceDiff(ctx, store, diff.Before, diff.After, sources)
			if err != nil {
				diff.Unavailable = err.Error()
			}
		}
		out.Repositories = append(out.Repositories, diff)
	}
	return out, ctx.Err()
}

var commitID = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func sourceDiff(ctx context.Context, store *runstore.Store, before, after string, sources []workflow.Artifact) (string, bool, error) {
	for _, pin := range []string{before, after} {
		if pin != "" && !commitID.MatchString(pin) {
			return "", false, errors.New("comparison requires immutable commit IDs")
		}
	}
	if before != "" && after != "" && len(before) != len(after) {
		return "", false, errors.New("repositories use different Git object formats")
	}
	root, err := os.MkdirTemp("", "envctl-review-")
	if err != nil {
		return "", false, err
	}
	defer os.RemoveAll(root)
	objects := filepath.Join(root, "objects")
	format := "sha1"
	if len(before) == 64 || len(after) == 64 {
		format = "sha256"
	}
	if _, _, err = git(ctx, root, "init", "--bare", "--template=", "--object-format="+format, objects); err != nil {
		return "", false, err
	}
	seen := map[string]bool{}
	for _, a := range sources {
		if a.Digest == "" || seen[a.Digest] {
			continue
		}
		seen[a.Digest] = true
		if a.MediaType != "application/x-git-bundle" || a.Size < 1 || a.Size > 64<<20 {
			return "", false, errors.New("source bundle has unsupported metadata")
		}
		raw, e := store.Artifact(a.Digest)
		if e != nil || int64(len(raw)) != a.Size {
			return "", false, errors.New("retained source bundle is missing or corrupt")
		}
		file := filepath.Join(root, "source.bundle")
		if err = os.WriteFile(file, raw, 0600); err != nil {
			return "", false, err
		}
		if _, _, err = git(ctx, objects, "bundle", "verify", file); err != nil {
			return "", false, errors.New("source bundle failed verification")
		}
		if _, _, err = git(ctx, objects, "bundle", "unbundle", file); err != nil {
			return "", false, errors.New("source bundle could not be imported")
		}
	}
	if len(seen) == 0 {
		return "", false, errors.New("no retained source bundle for this comparison")
	}
	for _, pin := range []string{before, after} {
		if pin == "" {
			continue
		}
		kind, _, e := git(ctx, objects, "cat-file", "-t", pin)
		if e != nil || strings.TrimSpace(kind) != "commit" {
			return "", false, errors.New("comparison commit is absent from retained source")
		}
	}
	if before == "" || after == "" {
		empty, _, e := git(ctx, objects, "hash-object", "-w", "-t", "tree", "--stdin")
		if e != nil {
			return "", false, e
		}
		if before == "" {
			before = strings.TrimSpace(empty)
		}
		if after == "" {
			after = strings.TrimSpace(empty)
		}
	}
	return git(ctx, objects, "diff", "--no-ext-diff", "--no-textconv", "--no-renames", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", before, after, "--")
}

// Never execute repository diff drivers, hooks, filters, or inherited Git
// configuration. Only local retained objects enter this temporary bare store.
func git(ctx context.Context, dir string, args ...string) (string, bool, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null"}, args...)...)
	cmd.Dir = dir
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "GIT_") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0")
	out := limitedOutput{limit: 1 << 20}
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return "", false, fmt.Errorf("retained-source Git %s failed", args[0])
	}
	return out.buf.String(), out.truncated, nil
}

type limitedOutput struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	keep := min(n, b.limit-b.buf.Len())
	_, _ = b.buf.Write(p[:keep])
	b.truncated = b.truncated || keep < n
	return n, nil
}
