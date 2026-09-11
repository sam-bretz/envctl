package repository

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// VerifyMerge checks a retained bundle outside the worker VM. A clean merge
// and a manually resolved conflict have the same contract: every input SHA
// must be an ancestor of the output. Squashing, replacing refs, grafts, or
// describing a merge in prose cannot substitute for that history. Executable
// checks and supervisor review separately judge the resulting content.
func VerifyMerge(ctx context.Context, bundle []byte, commit string, parents []string) error {
	if !sha.MatchString(commit) || len(parents) == 0 {
		return errors.New("merge verification requires immutable output and input commits")
	}
	for _, parent := range parents {
		if !sha.MatchString(parent) || len(parent) != len(commit) {
			return errors.New("merge input has an invalid object identity")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "envctl-merge-verify-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	filename := filepath.Join(dir, "source.bundle")
	if err = os.WriteFile(filename, bundle, 0600); err != nil {
		return err
	}
	env := []string{}
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "GIT_") {
			env = append(env, entry)
		}
	}
	env = append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_NO_REPLACE_OBJECTS=1")
	run := func(args ...string) error {
		args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "protocol.allow=never"}, args...)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir, cmd.Env = dir, env
		// No guest-controlled diagnostics are surfaced as coordinator evidence.
		return cmd.Run()
	}
	format := "sha1"
	if len(commit) == 64 {
		format = "sha256"
	}
	if err = run("init", "--bare", "--template=", "--object-format="+format, "."); err != nil {
		return errors.New("isolated merge verifier could not initialize Git")
	}
	if err = run("bundle", "verify", filename); err != nil {
		return errors.New("merge source is not a self-contained Git bundle")
	}
	if err = run("bundle", "unbundle", filename); err != nil {
		return errors.New("merge source objects could not be imported")
	}
	if err = run("fsck", "--strict", "--no-reflogs", commit); err != nil {
		return errors.New("merge source object graph is incomplete or invalid")
	}
	for _, parent := range parents {
		if err = run("merge-base", "--is-ancestor", parent, commit); err != nil {
			return fmt.Errorf("merge output %s does not retain required input %s", commit, parent)
		}
	}
	return nil
}
