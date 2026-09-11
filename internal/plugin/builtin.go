package plugin

import (
	"embed"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

//go:embed builtin/*/*
var builtinPackages embed.FS

func materializeBuiltin(cache, source string) (string, error) {
	name, ok := strings.CutPrefix(source, "builtin:")
	if !ok || name != "playwright" {
		return "", errors.New("unknown bundled invocation plugin")
	}
	if err := os.MkdirAll(cache, 0700); err != nil {
		return "", err
	}
	dir, err := os.MkdirTemp(cache, ".builtin-")
	if err != nil {
		return "", err
	}
	err = fs.WalkDir(builtinPackages, "builtin/"+name, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		raw, err := builtinPackages.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, entry.Name()), raw, 0600)
	})
	if err != nil {
		os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}
