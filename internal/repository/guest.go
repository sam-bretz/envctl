package repository

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

var executionID = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,100}$`)

// Guest owns source import and writer worktrees inside one revision's VM.
// Shared Git objects are immutable; each assignment has its own index, working
// files, branch, and preparation receipt. No host checkout is mounted.
type Guest struct {
	Executor          guestjob.Executor
	Runtime, Revision string
}

func (g Guest) root() (string, error) {
	if !executionID.MatchString(g.Revision) {
		return "", errors.New("invalid repository revision identity")
	}
	return "/work/envctl/revisions/" + g.Revision, nil
}
func (g Guest) SourceDir(id string) (string, error) {
	root, err := g.root()
	if err != nil {
		return "", err
	}
	if !safeID.MatchString(id) {
		return "", errors.New("invalid repository ID")
	}
	return path.Join(root, "sources", id), nil
}
func (g Guest) WorktreeDir(attempt, id string) (string, error) {
	root, err := g.root()
	if err != nil {
		return "", err
	}
	if !executionID.MatchString(attempt) || !safeID.MatchString(id) {
		return "", errors.New("invalid assignment repository identity")
	}
	return path.Join(root, "attempts", attempt, id), nil
}
func (g Guest) receipt(attempt, id string) string {
	return path.Join("/var/lib/envctl/repository-receipts", g.Revision, attempt, id)
}

func (g Guest) command(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer) error {
	var diagnostic bytes.Buffer
	if err := g.Executor.Exec(ctx, g.Runtime, vm.Command{Args: args, Stdin: stdin, Stdout: stdout, Stderr: &diagnostic}); err != nil {
		// These mechanical Git operations carry no credentials. Do not use this
		// helper for harness output or authenticated network operations.
		return fmt.Errorf("guest repository operation failed: %w: %s", err, strings.TrimSpace(diagnostic.String()))
	}
	return nil
}

func (g Guest) Import(ctx context.Context, source Source, archive string) error {
	destination, err := g.SourceDir(source.Repository.ID)
	if err != nil {
		return err
	}
	if !sha.MatchString(source.Repository.BaseSHA) {
		return errors.New("source import requires an immutable repository pin")
	}
	file, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer file.Close()
	h := sha256.New()
	if _, err = io.Copy(h, file); err != nil {
		return err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	binding := workflow.Digest(struct {
		Repository workflow.Repository
		Archive    string
	}{source.Repository, digest})
	script := `set -euo pipefail
destination="$1"; pin="$2"; digest="$3"; receipt="$4"; binding="$5"
install -d -m 700 "$(dirname "$receipt")"
# A dead coordinator's SSH session may leave this script running; serialize
# with any such orphan instead of racing it for the same destination.
exec 9>"$receipt.lock"
flock 9
if [ -f "$receipt" ]; then
 test "$(cat "$receipt")" = "$binding"
 test "$(sudo -u envctl-agent git -C "$destination" rev-parse HEAD)" = "$pin"
 exit 0
fi
archive=$(mktemp /var/lib/envctl/.source.XXXXXX)
stage=''
trap 'rm -f "$archive"; if [ -n "$stage" ]; then rm -rf "$stage"; fi' EXIT
cat > "$archive"
printf '%s  %s\n' "$digest" "$archive" | sha256sum --status -c -
install -d -o envctl-agent -g envctl-agent -m 700 "$(dirname "$destination")"
if [ ! -d "$destination" ]; then
 stage=$(mktemp -d "$(dirname "$destination")/.import.XXXXXX")
 chown envctl-agent:envctl-agent "$stage"
 sudo -u envctl-agent tar --no-same-owner -xf - -C "$stage" < "$archive"
 test "$(sudo -u envctl-agent git -C "$stage" rev-parse HEAD)" = "$pin"
 sudo -u envctl-agent git -C "$stage" config core.hooksPath /dev/null
 sudo -u envctl-agent git -C "$stage" config user.name 'envctl worker'
 sudo -u envctl-agent git -C "$stage" config user.email 'envctl@localhost'
 sudo -u envctl-agent git -C "$stage" config commit.gpgSign false
 mv "$stage" "$destination"; stage=''
fi
test "$(sudo -u envctl-agent git -C "$destination" rev-parse HEAD)" = "$pin"
printf '%s' "$binding" > "$receipt.tmp"
sync -f "$destination"
mv "$receipt.tmp" "$receipt"
sync -f "$(dirname "$receipt")"
`
	if err := g.command(ctx, []string{"sudo", "bash", "-c", script, "envctl-source-import", destination, source.Repository.BaseSHA, digest, g.receipt("sources", source.Repository.ID), binding}, file, nil); err != nil {
		return err
	}
	return g.captureObjects(ctx, source.Repository.ID, source.Repository.BaseSHA, destination)
}

// Assign is repeatable even after a worker has made uncommitted edits: once
// its preparation receipt exists, retrying dispatch never resets its files.
func (g Guest) Assign(ctx context.Context, attempt, id, pin string) (string, error) {
	destination, err := g.WorktreeDir(attempt, id)
	if err != nil {
		return "", err
	}
	source, err := g.SourceDir(id)
	if err != nil {
		return "", err
	}
	if !sha.MatchString(pin) {
		return "", errors.New("assignment requires an immutable input commit")
	}
	branch := "envctl/" + g.Revision + "/" + attempt + "/" + id
	script := `set -euo pipefail
source="$1"; destination="$2"; pin="$3"; branch="$4"; receipt="$5"
install -d -m 700 "$(dirname "$receipt")"
exec 9>"$receipt.lock"
flock 9
if [ -f "$receipt" ]; then
 test "$(cat "$receipt")" = "$pin"
 test "$(sudo -u envctl-agent git -C "$destination" rev-parse --path-format=absolute --git-common-dir)" = "$source/.git"
 exit 0
fi
install -d -o envctl-agent -g envctl-agent -m 700 "$(dirname "$destination")"
# No receipt means this path was never handed to a worker. A coordinator that
# died mid-preparation (its SSH session kills this script) can leave a partial
# checkout or a worktree Git still locks as "initializing"; neither can pass
# the checks below, so discard it and prepare again from the pin.
sudo -u envctl-agent git -C "$source" worktree unlock "$destination" >/dev/null 2>&1 || true
if [ -e "$destination" ]; then
 sudo -u envctl-agent git -C "$source" worktree remove --force --force "$destination" >/dev/null 2>&1 || true
 rm -rf --one-file-system -- "$destination"
fi
sudo -u envctl-agent git -C "$source" worktree prune
if sudo -u envctl-agent git -C "$source" show-ref --verify --quiet "refs/heads/$branch"; then
 sudo -u envctl-agent git -C "$source" branch -D "$branch" >/dev/null
fi
sudo -u envctl-agent env GIT_LFS_SKIP_SMUDGE=1 git -c core.hooksPath=/dev/null -C "$source" worktree add --detach "$destination" "$pin"
test "$(sudo -u envctl-agent git -C "$destination" rev-parse --path-format=absolute --git-common-dir)" = "$source/.git"
test "$(sudo -u envctl-agent git -C "$destination" rev-parse HEAD)" = "$pin"
test -z "$(sudo -u envctl-agent git -C "$destination" status --porcelain)"
if ! sudo -u envctl-agent git -C "$destination" symbolic-ref --quiet --short HEAD >/dev/null; then
 sudo -u envctl-agent git -c core.hooksPath=/dev/null -C "$destination" checkout -b "$branch"
fi
test "$(sudo -u envctl-agent git -C "$destination" symbolic-ref --short HEAD)" = "$branch"
sudo -u envctl-agent python3 -c "$6" hydrate "$destination" "$pin" "$7"
printf '%s' "$pin" > "$receipt.tmp"
sync -f "$destination"
mv "$receipt.tmp" "$receipt"; sync -f "$(dirname "$receipt")"
`
	objects, err := g.objectsPath(id, pin)
	if err != nil {
		return "", err
	}
	err = g.command(ctx, []string{"sudo", "bash", "-c", script, "envctl-worktree", source, destination, pin, branch, g.receipt(attempt, id), objectsProgram, objects}, nil, nil)
	return destination, err
}

// Capture records the actual worktree HEAD, committing any allowed changes.
// A read-only stage that changed its source is rejected before QA/approval.
func (g Guest) Capture(ctx context.Context, attempt, id string, allowChanges bool) (string, error) {
	dir, err := g.WorktreeDir(attempt, id)
	if err != nil {
		return "", err
	}
	mode := "readonly"
	if allowChanges {
		mode = "write"
	}
	var receipt bytes.Buffer
	if err = g.command(ctx, []string{"sudo", "cat", g.receipt(attempt, id)}, nil, &receipt); err != nil {
		return "", err
	}
	input := strings.TrimSpace(receipt.String())
	if !sha.MatchString(input) {
		return "", errors.New("assignment source receipt is invalid")
	}
	var out bytes.Buffer
	err = g.command(ctx, []string{"sudo", "-u", "envctl-agent", "python3", "-c", objectsProgram, "commit", dir, input, mode}, nil, &out)
	if err != nil {
		return "", err
	}
	commit := strings.TrimSpace(out.String())
	if !sha.MatchString(commit) {
		return "", errors.New("guest returned an invalid checkpoint commit")
	}
	return commit, nil
}

func (g Guest) ExportBundle(ctx context.Context, attempt, id, commit string, output io.Writer) error {
	dir, err := g.WorktreeDir(attempt, id)
	if err != nil {
		return err
	}
	if !sha.MatchString(commit) {
		return errors.New("bundle export requires immutable commit")
	}
	script := `set -euo pipefail
cd "$1"
test "$(git rev-parse HEAD)" = "$2"
file=$(mktemp /tmp/envctl-bundle.XXXXXX)
trap 'rm -f "$file"' EXIT
git -c core.hooksPath=/dev/null bundle create "$file" HEAD
git bundle verify "$file" >/dev/null 2>&1
cat "$file"
`
	return g.command(ctx, []string{"sudo", "-u", "envctl-agent", "bash", "-c", script, "envctl-bundle", dir, commit}, nil, output)
}

// RestoreBundle imports checkpoint objects into the revision's source store.
// It never changes a live writer's index, HEAD, or working files.
func (g Guest) RestoreBundle(ctx context.Context, id, commit string, bundle io.Reader) error {
	dir, err := g.SourceDir(id)
	if err != nil {
		return err
	}
	if !sha.MatchString(commit) {
		return errors.New("bundle restore requires immutable commit")
	}
	script := `set -euo pipefail
cd "$1"
exec 9>.git/envctl-restore.lock
flock 9
file=$(mktemp /tmp/envctl-restore.XXXXXX)
trap 'rm -f "$file"' EXIT
cat > "$file"
git bundle verify "$file" >/dev/null
git -c core.hooksPath=/dev/null fetch --no-recurse-submodules --no-tags "$file" "$2"
test "$(git cat-file -t "$2")" = commit
`
	return g.command(ctx, []string{"sudo", "-u", "envctl-agent", "bash", "-c", script, "envctl-restore", dir, commit}, bundle, nil)
}
