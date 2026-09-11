// Package workflow defines the versioned execution contract. It has no process,
// UI, or provider dependencies: agents cannot redefine its transition rules.
package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/sam-bretz/envctl/internal/manifest"
	"gopkg.in/yaml.v3"
)

var identifier = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

// Config is frozen into each invocation revision. Secret values never belong here.
type Config struct {
	Version      int               `yaml:"version" json:"version"`
	Project      string            `yaml:"project" json:"project"`
	Repositories []Repository      `yaml:"repositories" json:"repositories"`
	Runtime      RuntimeConfig     `yaml:"runtime" json:"runtime"`
	Stack        manifest.Stack    `yaml:"stack" json:"stack"`
	Ports        manifest.Ports    `yaml:"ports" json:"ports"`
	Expose       []manifest.Expose `yaml:"expose" json:"expose"`
	Plugins      []PluginRef       `yaml:"plugins" json:"plugins"`
	Data         DataConfig        `yaml:"data,omitempty" json:"data,omitzero"`
	Agents       AgentConfig       `yaml:"agents" json:"agents"`
	Workflow     Definition        `yaml:"workflow" json:"workflow"`
	Limits       Limits            `yaml:"limits" json:"limits"`
	Dir          string            `yaml:"-" json:"dir"`
}
type Repository struct {
	ID          string             `yaml:"id" json:"id"`
	URL         string             `yaml:"url" json:"url"`
	Ref         string             `yaml:"ref" json:"ref"`
	Branch      string             `yaml:"branch" json:"branch"`
	BaseSHA     string             `yaml:"base_sha,omitempty" json:"base_sha,omitempty"`
	Publication *PublicationTarget `yaml:"publication,omitempty" json:"publication,omitempty"`
}

// PublicationTarget is an explicit output destination, separate from a local
// source path or fetch URL. Base names a branch; readiness locks its exact SHA.
type PublicationTarget struct {
	Provider   string `yaml:"provider" json:"provider"`
	Repository string `yaml:"repository" json:"repository"`
	Base       string `yaml:"base" json:"base"`
}

func OutputBranch(repo Repository, revision string) string {
	prefix := repo.Branch
	if prefix == "" {
		prefix = "envctl/" + repo.ID
	}
	return prefix + "/" + revision
}

type RuntimeConfig struct {
	Provider    string `yaml:"provider" json:"provider"`
	Isolation   string `yaml:"isolation" json:"isolation"`
	CPUs        int    `yaml:"cpus" json:"cpus"`
	MemoryGiB   int    `yaml:"memory_gib" json:"memory_gib"`
	DiskGiB     int    `yaml:"disk_gib" json:"disk_gib"`
	Image       string `yaml:"image,omitempty" json:"image,omitempty"`
	ImageDigest string `yaml:"image_digest,omitempty" json:"image_digest,omitempty"`
}
type Limits struct {
	Parallel       int `yaml:"parallel" json:"parallel"`
	VMs            int `yaml:"vms" json:"vms"`
	MaxAttempts    int `yaml:"max_attempts" json:"max_attempts"`
	AttemptSeconds int `yaml:"attempt_seconds" json:"attempt_seconds"`
	// StallSeconds is how long a running agent may produce no harness output
	// before the coordinator intervenes. Zero means DefaultStallSeconds and is
	// omitted, so existing configuration identities are unchanged.
	StallSeconds int `yaml:"stall_seconds,omitempty" json:"stall_seconds,omitempty"`
}
type AgentConfig struct {
	Worker     Harness `yaml:"worker" json:"worker"`
	Supervisor Harness `yaml:"supervisor" json:"supervisor"`
}
type Harness struct {
	Kind       string   `yaml:"kind" json:"kind"`
	Version    string   `yaml:"version,omitempty" json:"version,omitempty"`
	Model      string   `yaml:"model,omitempty" json:"model,omitempty"`
	Credential string   `yaml:"credential,omitempty" json:"credential,omitempty"`
	Command    []string `yaml:"command,omitempty" json:"command,omitempty"`
}
type PluginRef struct {
	ID          string            `yaml:"id" json:"id"`
	Source      string            `yaml:"source" json:"source"`
	Version     string            `yaml:"version" json:"version"`
	Digest      string            `yaml:"digest,omitempty" json:"digest,omitempty"`
	Provides    []string          `yaml:"provides,omitempty" json:"provides,omitempty"`
	Requires    []string          `yaml:"requires,omitempty" json:"requires,omitempty"`
	Config      map[string]any    `yaml:"config,omitempty" json:"config,omitempty"`
	Credentials map[string]string `yaml:"credentials,omitempty" json:"credentials,omitempty"`
}
type Definition struct {
	Template string          `yaml:"template,omitempty" json:"template,omitempty"`
	Nodes    map[string]Node `yaml:"nodes" json:"nodes"`
}
type Node struct {
	Kind         string         `yaml:"kind" json:"kind"`
	Needs        []string       `yaml:"needs" json:"needs"`
	Requires     []string       `yaml:"requires" json:"requires"`
	Outputs      []string       `yaml:"outputs" json:"outputs"`
	OutputSchema map[string]any `yaml:"output_schema,omitempty" json:"output_schema,omitempty"`
	Checks       []Check        `yaml:"checks,omitempty" json:"checks,omitempty"`
	Gate         string         `yaml:"gate,omitempty" json:"gate,omitempty"`
	Prompt       string         `yaml:"prompt,omitempty" json:"prompt,omitempty"`
	Writes       []string       `yaml:"writes,omitempty" json:"writes,omitempty"`
	Priority     int            `yaml:"priority,omitempty" json:"priority,omitempty"`
	Join         *JoinPolicy    `yaml:"join,omitempty" json:"join,omitempty"`
	Limits       NodeLimits     `yaml:"limits,omitempty" json:"limits,omitzero"`
}

// NodeLimits overrides the revision-wide attempt budget for one node. Zero
// fields inherit Config.Limits; omitted overrides keep existing digests.
type NodeLimits struct {
	MaxAttempts    int `yaml:"max_attempts,omitempty" json:"max_attempts,omitempty"`
	AttemptSeconds int `yaml:"attempt_seconds,omitempty" json:"attempt_seconds,omitempty"`
	StallSeconds   int `yaml:"stall_seconds,omitempty" json:"stall_seconds,omitempty"`
}

// Upper bounds for node overrides; guest jobs refuse timeouts above one day.
const (
	MaxNodeAttempts       = 100
	MaxNodeAttemptSeconds = 86400
)

// Stall windows shorter than a minute would interrupt ordinary model latency.
const (
	DefaultStallSeconds = 600
	MinStallSeconds     = 60
)

func validStall(seconds int) bool {
	return seconds == 0 || (seconds >= MinStallSeconds && seconds <= MaxNodeAttemptSeconds)
}

// StallSeconds returns the effective no-output window for one node's agents.
func (c Config) StallSeconds(node string) int {
	if s := c.NodeLimits(node).StallSeconds; s > 0 {
		return s
	}
	return DefaultStallSeconds
}

// NodeLimits returns the effective limits for one node's assignments.
func (c Config) NodeLimits(node string) Limits {
	l := c.Limits
	n := c.Workflow.Nodes[node]
	if n.Limits.MaxAttempts > 0 {
		l.MaxAttempts = n.Limits.MaxAttempts
	}
	if n.Limits.AttemptSeconds > 0 {
		l.AttemptSeconds = n.Limits.AttemptSeconds
	}
	if n.Limits.StallSeconds > 0 {
		l.StallSeconds = n.Limits.StallSeconds
	}
	return l
}

// JoinPolicy names the input used to start explicit merge work. Repository
// outputs must retain every incoming commit in their ancestry. Dataset inputs
// are selected explicitly; databases are never combined by map iteration.
type JoinPolicy struct {
	Repositories map[string]string `yaml:"repositories,omitempty" json:"repositories,omitempty"`
	Datasets     map[string]string `yaml:"datasets,omitempty" json:"datasets,omitempty"`
}
type Check struct {
	Name           string         `yaml:"name" json:"name"`
	Repository     string         `yaml:"repository,omitempty" json:"repository,omitempty"`
	Command        []string       `yaml:"command" json:"command"`
	Plugin         string         `yaml:"plugin,omitempty" json:"plugin,omitempty"`
	Input          map[string]any `yaml:"input,omitempty" json:"input,omitempty"`
	TimeoutSeconds int            `yaml:"timeout_seconds,omitempty" json:"timeout_seconds,omitempty"`
}

func Feature() Definition {
	return Definition{Template: "feature", Nodes: map[string]Node{
		"task":            {Kind: "task", Outputs: []string{"task"}},
		"plan":            {Kind: "plan", Needs: []string{"task"}, Outputs: []string{"plan"}, Requires: []string{"harness.worker", "harness.supervisor", "runtime.compose", "repositories.readwrite", "publication.pr"}},
		"design":          {Kind: "design", Needs: []string{"plan"}, Outputs: []string{"design"}},
		"code":            {Kind: "code", Needs: []string{"design"}, Outputs: []string{"implementation"}, Writes: []string{"*"}},
		"qa":              {Kind: "qa", Needs: []string{"code"}, Outputs: []string{"test-results"}},
		"approved-change": {Kind: "change", Needs: []string{"qa"}, Outputs: []string{"change"}, Gate: "human"},
	}}
}

// Load accepts legacy manifests as inputs to a new workflow, without changing
// their behavior in existing lifecycle commands. V2 decoding rejects unknown keys.
func Load(dir string) (Config, error) {
	raw, err := os.ReadFile(filepath.Join(dir, manifest.FileName))
	if err != nil {
		return Config{}, err
	}
	var header struct {
		Version int `yaml:"version"`
	}
	if err = yaml.Unmarshal(raw, &header); err != nil {
		return Config{}, err
	}
	if header.Version == 0 || header.Version == 1 {
		m, e := manifest.Load(dir)
		if e != nil {
			return Config{}, e
		}
		c := Config{Version: 2, Project: m.Project, Dir: m.Dir, Stack: m.Stack, Ports: m.Ports, Expose: m.Expose, Repositories: []Repository{{ID: "main", URL: m.Dir, Ref: "HEAD"}}, Workflow: Feature()}
		c.defaults()
		return c, c.Validate()
	}
	c, err := Parse(raw)
	if err != nil {
		return Config{}, err
	}
	c.Dir, err = filepath.Abs(dir)
	return c, err
}

func Parse(raw []byte) (Config, error) {
	// Merge template defaults before strict decoding, preserving explicit empty
	// lists and empty gates instead of treating zero values as absent overrides.
	var doc map[string]any
	input := yaml.NewDecoder(bytes.NewReader(raw))
	if err := input.Decode(&doc); err != nil {
		return Config{}, err
	}
	var trailing any
	if err := input.Decode(&trailing); err != io.EOF {
		return Config{}, errors.New("expected one configuration document")
	}
	if wf, ok := doc["workflow"].(map[string]any); ok && wf["template"] == "feature" {
		baseBytes, _ := yaml.Marshal(Feature())
		var base map[string]any
		_ = yaml.Unmarshal(baseBytes, &base)
		merge(base, wf)
		doc["workflow"] = base
	}
	normalized, err := yaml.Marshal(doc)
	if err != nil {
		return Config{}, err
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(normalized))
	dec.KnownFields(true)
	if err = dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("workflow config: %w", err)
	}
	var extra any
	if e := dec.Decode(&extra); e != io.EOF {
		return Config{}, errors.New("expected one configuration document")
	}
	c.defaults()
	return c, c.Validate()
}
func merge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				merge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
}
func (c *Config) defaults() {
	if c.Runtime.Provider == "" {
		c.Runtime.Provider = "lima"
	}
	if c.Runtime.Provider == "local-vm" {
		c.Runtime.Provider = "lima"
	}
	if c.Runtime.Isolation == "" {
		c.Runtime.Isolation = "dedicated-vm"
	}
	if c.Runtime.CPUs == 0 {
		c.Runtime.CPUs = 2
	}
	if c.Runtime.MemoryGiB == 0 {
		c.Runtime.MemoryGiB = 4
	}
	if c.Runtime.DiskGiB == 0 {
		c.Runtime.DiskGiB = 30
	}
	if c.Limits.Parallel == 0 {
		c.Limits.Parallel = 1
	}
	if c.Limits.VMs == 0 {
		c.Limits.VMs = 2
	}
	if c.Limits.MaxAttempts == 0 {
		c.Limits.MaxAttempts = 12
	}
	if c.Limits.AttemptSeconds == 0 {
		c.Limits.AttemptSeconds = 1800
	}
	if c.Agents.Worker.Kind == "" {
		c.Agents.Worker.Kind = "codex"
	}
	if c.Agents.Supervisor.Kind == "" {
		c.Agents.Supervisor.Kind = c.Agents.Worker.Kind
	}
	for _, h := range []*Harness{&c.Agents.Worker, &c.Agents.Supervisor} {
		if h.Version == "" {
			h.Version = DefaultHarnessVersion(h.Kind)
		}
	}
	for i := range c.Repositories {
		if c.Repositories[i].Ref == "" {
			c.Repositories[i].Ref = "HEAD"
		}
	}
}
func (c Config) Validate() error {
	if err := c.Data.Validate(c.Repositories); err != nil {
		return err
	}
	var errs []error
	if _, err := json.Marshal(c); err != nil {
		return fmt.Errorf("configuration must contain JSON-compatible values: %w", err)
	}
	if c.Version != 2 {
		errs = append(errs, fmt.Errorf("unsupported workflow version %d", c.Version))
	}
	if !identifier.MatchString(c.Project) {
		errs = append(errs, errors.New("project must be a lowercase identifier"))
	}
	if c.Runtime.Isolation != "dedicated-vm" {
		errs = append(errs, errors.New("workflow execution requires dedicated-vm isolation"))
	}
	if c.Runtime.CPUs < 1 || c.Runtime.MemoryGiB < 1 || c.Runtime.DiskGiB < 4 {
		errs = append(errs, errors.New("invalid VM resources"))
	}
	if c.Limits.Parallel < 1 || c.Limits.VMs < 1 || c.Limits.MaxAttempts < 1 || c.Limits.AttemptSeconds < 1 {
		errs = append(errs, errors.New("limits must be positive"))
	}
	if !validStall(c.Limits.StallSeconds) {
		errs = append(errs, fmt.Errorf("limits.stall_seconds must be between %d and %d", MinStallSeconds, MaxNodeAttemptSeconds))
	}
	if c.Limits.Parallel > 1 && c.Limits.VMs < 2 {
		errs = append(errs, errors.New("parallel execution requires capacity for the revision VM and at least one child VM"))
	}
	seen := map[string]bool{}
	if len(c.Repositories) == 0 {
		errs = append(errs, errors.New("repositories must not be empty"))
	}
	for _, r := range c.Repositories {
		if !identifier.MatchString(r.ID) || seen[r.ID] || strings.TrimSpace(r.URL) == "" {
			errs = append(errs, fmt.Errorf("invalid or duplicate repository %q", r.ID))
		}
		seen[r.ID] = true
	}
	plugins := map[string]bool{}
	for _, p := range c.Plugins {
		if !identifier.MatchString(p.ID) || plugins[p.ID] || p.Source == "" || p.Version == "" {
			errs = append(errs, fmt.Errorf("plugin %q requires unique id, source and version", p.ID))
		}
		plugins[p.ID] = true
		for name, ref := range p.Credentials {
			if !strings.HasPrefix(ref, "env:") && !strings.HasPrefix(ref, "file:") {
				errs = append(errs, fmt.Errorf("plugin %s credential %s must be an env: or file: reference", p.ID, name))
			}
		}
	}
	if err := c.Workflow.Validate(); err != nil {
		errs = append(errs, err)
	} else if err := c.validateJoinOwnership(); err != nil {
		errs = append(errs, err)
	}
	for id, n := range c.Workflow.Nodes {
		if n.Join != nil {
			for repo := range n.Join.Repositories {
				if !seen[repo] {
					errs = append(errs, fmt.Errorf("node %s join references unknown repository %s", id, repo))
				}
			}
			for dataset := range n.Join.Datasets {
				found := false
				for _, spec := range c.Data.Datasets {
					found = found || spec.ID == dataset
				}
				if !found {
					errs = append(errs, fmt.Errorf("node %s join references unknown dataset %s", id, dataset))
				}
			}
		}
		for _, check := range n.Checks {
			if check.Repository != "" && !seen[check.Repository] {
				errs = append(errs, fmt.Errorf("node %s check references unknown repository %s", id, check.Repository))
			}
		}
	}
	return errors.Join(errs...)
}
func (d Definition) Validate() error {
	if d.Template != "" && d.Template != "feature" {
		return fmt.Errorf("unknown workflow template %q", d.Template)
	}
	if len(d.Nodes) == 0 {
		return errors.New("workflow has no nodes")
	}
	for id, n := range d.Nodes {
		if !identifier.MatchString(id) {
			return fmt.Errorf("invalid node id %q", id)
		}
		if !slices.Contains([]string{"task", "plan", "design", "code", "qa", "change", "custom"}, n.Kind) {
			return fmt.Errorf("node %s: invalid kind %q", id, n.Kind)
		}
		if n.Gate != "" && n.Gate != "human" {
			return fmt.Errorf("node %s: gate must be human or empty", id)
		}
		if len(n.Outputs) == 0 {
			return fmt.Errorf("node %s needs required output artifacts", id)
		}
		if n.Limits.MaxAttempts < 0 || n.Limits.MaxAttempts > MaxNodeAttempts || n.Limits.AttemptSeconds < 0 || n.Limits.AttemptSeconds > MaxNodeAttemptSeconds {
			return fmt.Errorf("node %s: limits must be positive, with at most %d attempts and %d attempt seconds", id, MaxNodeAttempts, MaxNodeAttemptSeconds)
		}
		if !validStall(n.Limits.StallSeconds) {
			return fmt.Errorf("node %s: stall_seconds must be between %d and %d", id, MinStallSeconds, MaxNodeAttemptSeconds)
		}
		unique := map[string]bool{}
		for _, o := range n.Outputs {
			if !identifier.MatchString(o) || unique[o] {
				return fmt.Errorf("node %s: invalid or duplicate output %q", id, o)
			}
			unique[o] = true
		}
		deps := map[string]bool{}
		for _, dep := range n.Needs {
			if _, ok := d.Nodes[dep]; !ok {
				return fmt.Errorf("node %s: unknown dependency %s", id, dep)
			}
			if deps[dep] {
				return fmt.Errorf("node %s: duplicate dependency %s", id, dep)
			}
			deps[dep] = true
		}
		if n.Join != nil {
			if len(n.Needs) < 2 || len(n.Join.Repositories)+len(n.Join.Datasets) == 0 {
				return fmt.Errorf("node %s: join requires multiple dependencies and an explicit policy", id)
			}
			for repo, parent := range n.Join.Repositories {
				if !deps[parent] || (!slices.Contains(n.Writes, repo) && !slices.Contains(n.Writes, "*")) || len(n.Checks) == 0 || (n.Kind != "code" && n.Kind != "custom") {
					return fmt.Errorf("node %s: repository join %s requires a direct input, writable ownership, executable checks and code/custom kind", id, repo)
				}
			}
			for dataset, parent := range n.Join.Datasets {
				if !deps[parent] {
					return fmt.Errorf("node %s: dataset join %s must name a direct input", id, dataset)
				}
			}
		}
		names := map[string]bool{}
		for _, c := range n.Checks {
			command := len(c.Command) > 0 && c.Command[0] != ""
			plugin := identifier.MatchString(c.Plugin)
			if c.Name == "" || names[c.Name] || command == plugin || (c.Plugin != "" && !plugin) || (len(c.Command) > 0 && !command) || (len(c.Input) > 0 && !plugin) || c.TimeoutSeconds < 0 {
				return fmt.Errorf("node %s: invalid check %q", id, c.Name)
			}
			names[c.Name] = true
		}
	}
	if _, err := d.Order(); err != nil {
		return err
	}
	plan := ""
	task := ""
	for id, n := range d.Nodes {
		if n.Kind == "plan" {
			if plan != "" {
				return errors.New("exactly one Plan node required")
			}
			plan = id
		}
		if n.Kind == "task" {
			if task != "" {
				return errors.New("exactly one Task node required")
			}
			task = id
		}
	}
	if plan == "" || task == "" {
		return errors.New("Task and Plan nodes are required")
	}
	if len(d.Nodes[task].Needs) != 0 {
		return errors.New("Task must be the root")
	}
	if !d.Descendants(task)[plan] {
		return errors.New("Plan must depend on Task")
	}
	afterPlan := d.Descendants(plan)
	for id := range d.Nodes {
		if id != task && id != plan && !afterPlan[id] {
			return fmt.Errorf("node %s can bypass Plan readiness", id)
		}
	}
	return nil
}

// Order uses stable topological ordering; map iteration cannot alter dispatch.
func (d Definition) Order() ([]string, error) {
	degree := map[string]int{}
	for id, n := range d.Nodes {
		degree[id] = len(n.Needs)
	}
	var out []string
	for len(degree) > 0 {
		ready := []string{}
		for id, n := range degree {
			if n == 0 {
				ready = append(ready, id)
			}
		}
		slices.Sort(ready)
		if len(ready) == 0 {
			return nil, errors.New("workflow contains a dependency cycle")
		}
		for _, id := range ready {
			out = append(out, id)
			delete(degree, id)
			for other := range degree {
				if slices.Contains(d.Nodes[other].Needs, id) {
					degree[other]--
				}
			}
		}
	}
	return out, nil
}
func (d Definition) Descendants(id string) map[string]bool {
	out := map[string]bool{}
	queue := []string{id}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for child, n := range d.Nodes {
			if !out[child] && slices.Contains(n.Needs, p) {
				out[child] = true
				queue = append(queue, child)
			}
		}
	}
	return out
}
func (d Definition) PlanID() string {
	for id, n := range d.Nodes {
		if n.Kind == "plan" {
			return id
		}
	}
	return ""
}
func Clone[T any](v T) T {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	var out T
	if err = json.Unmarshal(b, &out); err != nil {
		panic(err)
	}
	return out
}
