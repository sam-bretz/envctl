package plugin

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sam-bretz/envctl/internal/workflow"
	"gopkg.in/yaml.v3"
)

// Builtins are supplied by the coordinator and cannot be replaced by an
// invocation executable claiming to have performed authoritative verification.
func Builtins() map[string]string {
	return map[string]string{"harness.worker": "builtin", "harness.supervisor": "builtin", "repositories.readwrite": "builtin", "runtime.compose": "builtin", "publication.pr": "builtin", "workflow.checks": "builtin", "dataset.restore": "builtin"}
}

// Freeze retains the exact package bytes before admitting an invocation. Only
// references enter configuration; credentials are resolved at execution time.
// Cache installation is atomic and safe to repeat before a state transaction.
func Freeze(cache string, c workflow.Config) (workflow.Config, Lock, error) {
	c = workflow.Clone(c)
	var bindings []Binding
	for i, ref := range c.Plugins {
		if strings.HasPrefix(ref.Source, "builtin:") {
			dir, err := materializeBuiltin(cache, ref.Source)
			if err != nil {
				return c, Lock{}, err
			}
			defer os.RemoveAll(dir)
			ref.Source = dir
		}
		b, err := Load(c.Dir, ref)
		if err != nil {
			return c, Lock{}, err
		}
		dest := filepath.Join(cache, b.Digest)
		if err = retain(b, dest); err != nil {
			return c, Lock{}, err
		}
		b.SourceDir, b.Ref.Source = dest, dest
		if len(b.Ref.Provides) == 0 {
			b.Ref.Provides = append([]string(nil), b.Descriptor.Provides...)
		}
		b.Ref.Requires = append([]string(nil), b.Descriptor.Requires...)
		c.Plugins[i] = b.Ref
		bindings = append(bindings, b)
	}
	lock, err := Resolve(bindings, Builtins())
	return c, lock, err
}

func retain(b Binding, dest string) error {
	if digest, err := SourceDigest(dest); err == nil {
		if digest != b.Digest {
			return errors.New("retained plugin package is corrupt")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(filepath.Dir(dest), ".package-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	err = filepath.WalkDir(b.SourceDir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(b.SourceDir, path)
		if err != nil {
			return err
		}
		out := filepath.Join(tmp, rel)
		if entry.IsDir() {
			return os.MkdirAll(out, 0700)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return errors.New("plugin source changed while packaging")
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, in)
		err = errors.Join(copyErr, f.Chmod(info.Mode().Perm()), f.Sync(), f.Close())
		return err
	})
	if err != nil {
		return err
	}
	digest, err := SourceDigest(tmp)
	if err != nil {
		return err
	}
	if digest != b.Digest {
		return errors.New("plugin source changed while packaging")
	}
	if err = os.Rename(tmp, dest); err != nil {
		if existing, check := SourceDigest(dest); check != nil || existing != b.Digest {
			return err
		}
	}
	d, err := os.Open(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// ReadRef reads the invocation attachment file shared by CLI and TUI.
func ReadRef(path string) (workflow.PluginRef, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return workflow.PluginRef{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return workflow.PluginRef{}, err
	}
	return decodeRef(raw, filepath.Dir(path))
}

func decodeRef(raw []byte, dir string) (workflow.PluginRef, error) {
	var ref workflow.PluginRef
	d := yaml.NewDecoder(bytes.NewReader(raw))
	d.KnownFields(true)
	if err := d.Decode(&ref); err != nil {
		return ref, err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return ref, errors.New("expected one plugin reference")
	}
	if ref.Source != "" && !filepath.IsAbs(ref.Source) && !strings.HasPrefix(ref.Source, "builtin:") {
		ref.Source = filepath.Join(dir, ref.Source)
	}
	for k, v := range ref.Credentials {
		if path, ok := strings.CutPrefix(v, "file:"); ok && !filepath.IsAbs(path) {
			ref.Credentials[k] = "file:" + filepath.Join(dir, path)
		}
	}
	return ref, nil
}
