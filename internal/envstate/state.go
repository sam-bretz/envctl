// Package envstate records what envctl knows about an environment beyond what
// Docker can tell it: its name, the branch it is linked to, the dataset it was
// seeded from, its parent, and whether it was created explicitly (kept).
// Locally this lives at .envctl/<env>/env.json in the worktree.
package envstate

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// Dir is the directory under the worktree root that holds rendered files.
const Dir = ".envctl"

// FileName is the state document inside .envctl/<env>/.
const FileName = "env.json"

// State is the per-environment document.
type State struct {
	Name      string    `json:"name"`
	Backend   string    `json:"backend"`
	Branch    string    `json:"branch,omitempty"`  // linked branch; empty = unlinked
	Dataset   string    `json:"dataset,omitempty"` // reserved for milestone 2
	Parent    string    `json:"parent,omitempty"`  // parent environment in the graph
	Kept      bool      `json:"kept"`              // created explicitly; CI never destroys it
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Path returns the state file path for env under root.
func Path(root, env string) string {
	return filepath.Join(root, Dir, env, FileName)
}

// Load reads the state for env, or returns os.ErrNotExist.
func Load(root, env string) (*State, error) {
	raw, err := os.ReadFile(Path(root, env))
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Save writes the state, stamping timestamps.
func Save(root string, s *State) error {
	now := time.Now().UTC()
	if s.CreatedAt.IsZero() {
		s.CreatedAt = now
	}
	s.UpdatedAt = now
	dir := filepath.Dir(Path(root, s.Name))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(Path(root, s.Name), append(b, '\n'), 0o644)
}

// Upsert loads the existing state if any and applies fn to it before saving.
// fn sees a fresh State{Name: env, Backend: backend} when none exists.
func Upsert(root, env, backend string, fn func(*State)) (*State, error) {
	s, err := Load(root, env)
	switch {
	case errors.Is(err, os.ErrNotExist):
		s = &State{Name: env, Backend: backend}
	case err != nil:
		return nil, err
	}
	if fn != nil {
		fn(s)
	}
	return s, Save(root, s)
}

// Remove deletes the whole .envctl/<env>/ directory.
func Remove(root, env string) error {
	return os.RemoveAll(filepath.Join(root, Dir, env))
}

// List returns every recorded environment under root, sorted by name.
func List(root string) ([]*State, error) {
	entries, err := os.ReadDir(filepath.Join(root, Dir))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*State
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		s, err := Load(root, e.Name())
		if err != nil {
			continue // a rendered dir without env.json (pre-identity render)
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
