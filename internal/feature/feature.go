// Package feature derives the default environment name (a slug of the git
// branch of the current worktree) and the compose project name from it. The
// branch is an attribute of an environment, not its identity; see the
// environment identity design note.
package feature

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// MaxSlugLen keeps project names comfortably inside Docker's limits and
// readable in DNS labels (service.project.orb.local).
const MaxSlugLen = 40

var (
	nonSlug   = regexp.MustCompile(`[^a-z0-9]+`)
	multiDash = regexp.MustCompile(`-{2,}`)
)

// Slugify turns a branch name (or any string) into a Docker/DNS safe slug.
func Slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = nonSlug.ReplaceAllString(s, "-")
	s = multiDash.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > MaxSlugLen {
		s = strings.Trim(s[:MaxSlugLen], "-")
	}
	if s == "" {
		s = "default"
	}
	return s
}

// ProjectName composes the compose project name from the manifest prefix and
// the feature slug.
func ProjectName(prefix, slug string) string {
	return prefix + "-" + slug
}

// Branch returns the checked-out branch of the worktree containing dir, or ""
// on a detached HEAD.
func Branch(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("not a git worktree: %w", err)
	}
	b := strings.TrimSpace(out)
	if b == "HEAD" {
		return "", nil
	}
	return b, nil
}

// Detect returns the default environment name for the worktree containing
// dir: the branch slug when on a branch, otherwise the worktree directory name.
func Detect(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", fmt.Errorf("not a git worktree: %w", err)
	}
	branch := strings.TrimSpace(out)
	if branch == "" || branch == "HEAD" {
		top, err := git(ctx, dir, "rev-parse", "--show-toplevel")
		if err != nil {
			return "", err
		}
		return Slugify(filepath.Base(strings.TrimSpace(top))), nil
	}
	return Slugify(branch), nil
}

// Toplevel returns the root of the worktree containing dir.
func Toplevel(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	b, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return "", err
	}
	return string(b), nil
}
