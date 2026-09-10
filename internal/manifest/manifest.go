// Package manifest reads envctl.yaml, the per-project contract between an
// application repository and the harness.
package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"gopkg.in/yaml.v3"
)

// FileName is the manifest file looked up at the repository root.
const FileName = "envctl.yaml"

// PortMode controls how published host ports are rewritten per environment.
type PortMode string

const (
	// PortModeAuto picks Domains on OrbStack and Registry elsewhere.
	PortModeAuto PortMode = "auto"
	// PortModeDomains strips host ports and relies on per-container DNS
	// (OrbStack's service.project.orb.local).
	PortModeDomains PortMode = "domains"
	// PortModeRegistry allocates a stable host port per project/service/target
	// from a local registry.
	PortModeRegistry PortMode = "registry"
)

// Manifest is the parsed envctl.yaml.
type Manifest struct {
	Version int      `yaml:"version"`
	Project string   `yaml:"project"`
	Stack   Stack    `yaml:"stack"`
	Ports   Ports    `yaml:"ports"`
	Expose  []Expose `yaml:"expose"`

	// Dir is the directory containing the manifest (the repository root).
	Dir string `yaml:"-"`
}

// Stack names the compose inputs.
type Stack struct {
	Files    []string `yaml:"files"`
	EnvFiles []string `yaml:"env_files"`
	Profiles []string `yaml:"profiles"`
}

// Ports configures host port handling.
type Ports struct {
	Mode  PortMode `yaml:"mode"`
	Range [2]int   `yaml:"range"`
}

// Expose marks a service (and container port) as a user-facing entrypoint so
// status output can print a URL for it.
type Expose struct {
	Service string `yaml:"service"`
	Port    int    `yaml:"port"`
	Scheme  string `yaml:"scheme"`
	Path    string `yaml:"path"`
}

var projectRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,15}$`)

// Load reads and validates the manifest at dir/envctl.yaml.
func Load(dir string) (*Manifest, error) {
	path := filepath.Join(dir, FileName)
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := yaml.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	m.Dir = dir
	m.applyDefaults()
	if err := m.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &m, nil
}

// Find walks up from start looking for envctl.yaml and loads it.
func Find(start string) (*Manifest, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return nil, err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, FileName)); err == nil {
			return Load(dir)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, fmt.Errorf("no %s found in %s or any parent", FileName, start)
		}
		dir = parent
	}
}

func (m *Manifest) applyDefaults() {
	if m.Version == 0 {
		m.Version = 1
	}
	if m.Ports.Mode == "" {
		m.Ports.Mode = PortModeAuto
	}
	if m.Ports.Range == [2]int{} {
		m.Ports.Range = [2]int{41000, 49999}
	}
	for i := range m.Expose {
		if m.Expose[i].Scheme == "" {
			m.Expose[i].Scheme = "http"
		}
		if m.Expose[i].Path == "" {
			m.Expose[i].Path = "/"
		}
	}
}

func (m *Manifest) validate() error {
	var errs []error
	if m.Version != 1 {
		errs = append(errs, fmt.Errorf("unsupported version %d (want 1)", m.Version))
	}
	if !projectRE.MatchString(m.Project) {
		errs = append(errs, fmt.Errorf("project %q must match %s", m.Project, projectRE))
	}
	if len(m.Stack.Files) == 0 {
		errs = append(errs, errors.New("stack.files must list at least one compose file"))
	}
	for _, f := range m.Stack.Files {
		if _, err := os.Stat(filepath.Join(m.Dir, f)); err != nil {
			errs = append(errs, fmt.Errorf("stack file %s: %w", f, err))
		}
	}
	switch m.Ports.Mode {
	case PortModeAuto, PortModeDomains, PortModeRegistry:
	default:
		errs = append(errs, fmt.Errorf("ports.mode %q must be auto, domains or registry", m.Ports.Mode))
	}
	if m.Ports.Range[0] < 1024 || m.Ports.Range[1] > 65535 || m.Ports.Range[0] >= m.Ports.Range[1] {
		errs = append(errs, fmt.Errorf("ports.range %v must be within 1024-65535 and ascending", m.Ports.Range))
	}
	return errors.Join(errs...)
}

// StackPaths returns the compose files as absolute paths.
func (m *Manifest) StackPaths() []string {
	out := make([]string, 0, len(m.Stack.Files))
	for _, f := range m.Stack.Files {
		out = append(out, filepath.Join(m.Dir, f))
	}
	return out
}

// EnvFilePaths returns the env files as absolute paths, skipping missing ones.
func (m *Manifest) EnvFilePaths() []string {
	var out []string
	for _, f := range m.Stack.EnvFiles {
		p := filepath.Join(m.Dir, f)
		if _, err := os.Stat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
