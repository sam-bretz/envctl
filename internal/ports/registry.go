// Package ports allocates stable host ports per environment from a local
// registry, for hosts without per-container DNS.
package ports

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
)

// Key identifies one published port: project + service + container port.
type Key struct {
	Project string
	Service string
	Target  int
}

func (k Key) String() string { return fmt.Sprintf("%s/%s/%d", k.Project, k.Service, k.Target) }

// Registry is a JSON file mapping keys to host ports.
type Registry struct {
	path  string
	lo    int
	hi    int
	Alloc map[string]int `json:"allocations"`
}

// DefaultPath is ~/.config/envctl/ports.json (honours XDG_CONFIG_HOME).
func DefaultPath() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "envctl", "ports.json"), nil
}

// Open loads the registry at path, creating an empty one if absent.
func Open(path string, lo, hi int) (*Registry, error) {
	r := &Registry{path: path, lo: lo, hi: hi, Alloc: map[string]int{}}
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return r, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(raw, r); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if r.Alloc == nil {
		r.Alloc = map[string]int{}
	}
	return r, nil
}

// Get returns the port allocated for key, allocating a free one if needed.
// A port is considered taken if it is in the registry or currently bound.
func (r *Registry) Get(k Key) (int, error) {
	if p, ok := r.Alloc[k.String()]; ok {
		return p, nil
	}
	used := map[int]bool{}
	for _, p := range r.Alloc {
		used[p] = true
	}
	for p := r.lo; p <= r.hi; p++ {
		if used[p] || !free(p) {
			continue
		}
		r.Alloc[k.String()] = p
		return p, nil
	}
	return 0, fmt.Errorf("no free port in %d-%d", r.lo, r.hi)
}

// Release drops every allocation for project.
func (r *Registry) Release(project string) {
	for k := range r.Alloc {
		if len(k) > len(project) && k[:len(project)+1] == project+"/" {
			delete(r.Alloc, k)
		}
	}
}

// Save writes the registry back to disk.
func (r *Registry) Save() error {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, b, 0o644)
}

// Keys returns allocations sorted for stable output.
func (r *Registry) Keys() []string {
	ks := make([]string, 0, len(r.Alloc))
	for k := range r.Alloc {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

func free(port int) bool {
	l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return false
	}
	_ = l.Close()
	return true
}
