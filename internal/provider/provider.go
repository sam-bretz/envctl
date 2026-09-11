// Package provider defines the environment contract every backend implements.
// The workflow engine and the CLI only ever talk to this interface.
package provider

import "context"

// Backend names an implementation.
type Backend string

const (
	Local Backend = "local"
	// VM is reserved for milestone 3 (one EC2 instance per feature).
	VM Backend = "vm"
)

// Spec is everything needed to converge an environment.
type Spec struct {
	Env      string // environment name, e.g. feat-imported-traffic-pricing (defaults to the branch slug)
	Branch   string // linked branch, e.g. feat/imported-traffic-pricing; empty when unlinked
	Kept     bool   // created explicitly with `env create`; CI never destroys it
	Project  string // compose project name, e.g. mg-feat-imported-traffic-pricing
	Backend  Backend
	ImageTag string // optional; local builds when empty
	Dataset  string // reserved for milestone 2
	Parent   string // parent environment name in the graph, if any
	Build    bool   // pass --build to compose up
	Wait     bool   // pass --wait to compose up
}

// Endpoint is how a human or agent reaches a service.
type Endpoint struct {
	Service string `json:"service"`
	Target  int    `json:"target"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	URL     string `json:"url,omitempty"`
}

// Service is the runtime state of one compose service.
type Service struct {
	Name   string `json:"name"`
	State  string `json:"state"`
	Health string `json:"health,omitempty"`
}

// Status is the reconciled view of an environment.
type Status struct {
	Name      string     `json:"name"`    // environment name
	Feature   string     `json:"feature"` // deprecated alias of Name, kept for one release
	Branch    string     `json:"branch,omitempty"`
	Kept      bool       `json:"kept"`
	Project   string     `json:"project"`
	Backend   Backend    `json:"backend"`
	Running   bool       `json:"running"`
	Services  []Service  `json:"services"`
	Endpoints []Endpoint `json:"endpoints"`
	EnvLines  []string   `json:"env"` // KEY=VALUE lines for host tooling
	Rendered  string     `json:"rendered"`
}

// Provider is the environment lifecycle.
type Provider interface {
	Up(ctx context.Context, spec Spec) (*Status, error)
	Down(ctx context.Context, spec Spec, volumes bool) error
	Stop(ctx context.Context, spec Spec) error
	Start(ctx context.Context, spec Spec) error
	Status(ctx context.Context, spec Spec) (*Status, error)
	Logs(ctx context.Context, spec Spec, follow bool, services ...string) error
	Exec(ctx context.Context, spec Spec, service string, args ...string) error
	Render(ctx context.Context, spec Spec) (*Status, error)
}
