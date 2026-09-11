package repository

import (
	"context"
	_ "embed"
	"errors"
	"io"
	"path"
)

const ObjectsMediaType = "application/vnd.envctl.git-objects+tar"

//go:embed objects.py
var objectsProgram string

func (g Guest) objectsPath(id, commit string) (string, error) {
	if !sha.MatchString(commit) {
		return "", errors.New("source objects require an immutable commit")
	}
	source, err := g.SourceDir(id)
	if err != nil {
		return "", err
	}
	return path.Join(source, ".git", "envctl-objects", commit+".tar"), nil
}
func (g Guest) captureObjects(ctx context.Context, id, commit, directory string) error {
	filename, err := g.objectsPath(id, commit)
	if err != nil {
		return err
	}
	return g.command(ctx, []string{"sudo", "-u", "envctl-agent", "python3", "-c", objectsProgram, "capture", directory, commit, filename}, nil, nil)
}
func (g Guest) ExportObjects(ctx context.Context, attempt, id, commit string, out io.Writer) error {
	directory, err := g.WorktreeDir(attempt, id)
	if err != nil {
		return err
	}
	if err = g.captureObjects(ctx, id, commit, directory); err != nil {
		return err
	}
	filename, err := g.objectsPath(id, commit)
	if err != nil {
		return err
	}
	return g.command(ctx, []string{"sudo", "-u", "envctl-agent", "cat", filename}, nil, out)
}
func (g Guest) RestoreObjects(ctx context.Context, id, commit string, input io.Reader) error {
	source, err := g.SourceDir(id)
	if err != nil {
		return err
	}
	filename, err := g.objectsPath(id, commit)
	if err != nil {
		return err
	}
	return g.command(ctx, []string{"sudo", "-u", "envctl-agent", "python3", "-c", objectsProgram, "restore", source, commit, filename}, input, nil)
}
