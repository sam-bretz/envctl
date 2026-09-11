// Package local runs an environment as an isolated compose project on the
// developer's Docker host (OrbStack, Docker Desktop, Colima, Linux engine).
package local

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sam-bretz/envctl/internal/compose"
	"github.com/sam-bretz/envctl/internal/dockerx"
	"github.com/sam-bretz/envctl/internal/envstate"
	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/ports"
	"github.com/sam-bretz/envctl/internal/provider"
)

// Provider is the local backend.
type Provider struct {
	Manifest     *manifest.Manifest
	Host         dockerx.Host
	RegistryPath string

	// hostErr records a failed Docker detection. Render works without Docker
	// (CI validation, offline inspection); every other operation reports it.
	hostErr error
}

// New detects the Docker host and returns a local provider. A missing daemon
// is not fatal here so that Render can run on hosts without Docker.
func New(ctx context.Context, m *manifest.Manifest) (*Provider, error) {
	rp, err := ports.DefaultPath()
	if err != nil {
		return nil, err
	}
	p := &Provider{Manifest: m, RegistryPath: rp}
	p.Host, p.hostErr = dockerx.DetectHost(ctx)
	return p, nil
}

func (p *Provider) requireDocker() error {
	return p.hostErr
}

// Mode resolves the manifest's port mode against the detected host.
func (p *Provider) Mode() compose.Mode {
	switch p.Manifest.Ports.Mode {
	case manifest.PortModeDomains:
		return compose.ModeDomains
	case manifest.PortModeRegistry:
		return compose.ModeRegistry
	default:
		if p.Host.OrbStack {
			return compose.ModeDomains
		}
		return compose.ModeRegistry
	}
}

// OutDir is where rendered files for an environment live: <repo>/.envctl/<env>/.
func (p *Provider) OutDir(spec provider.Spec) string {
	return filepath.Join(p.Manifest.Dir, envstate.Dir, spec.Env)
}

// record writes env.json, preserving fields set by `env create` / `env link`.
func (p *Provider) record(spec provider.Spec) (*envstate.State, error) {
	return envstate.Upsert(p.Manifest.Dir, spec.Env, string(provider.Local), func(s *envstate.State) {
		if spec.Branch != "" {
			s.Branch = spec.Branch
		}
		if spec.Kept {
			s.Kept = true
		}
	})
}

func (p *Provider) render(ctx context.Context, spec provider.Spec) (*compose.Result, *ports.Registry, error) {
	mode := p.Mode()
	var reg *ports.Registry
	if mode == compose.ModeRegistry {
		r, err := ports.Open(p.RegistryPath, p.Manifest.Ports.Range[0], p.Manifest.Ports.Range[1])
		if err != nil {
			return nil, nil, err
		}
		reg = r
	}
	extra := map[string]string{}
	if spec.ImageTag != "" {
		extra["ENVCTL_IMAGE_TAG"] = spec.ImageTag
	}
	st, err := p.record(spec)
	if err != nil {
		return nil, nil, err
	}
	res, err := compose.Render(ctx, compose.Options{
		Manifest: p.Manifest,
		Env:      spec.Env,
		Branch:   st.Branch,
		Project:  spec.Project,
		Backend:  string(provider.Local),
		Mode:     mode,
		Registry: reg,
		OutDir:   p.OutDir(spec),
		ExtraEnv: extra,
	})
	if err != nil {
		return nil, nil, err
	}
	if reg != nil {
		if err := reg.Save(); err != nil {
			return nil, nil, err
		}
	}
	return res, reg, nil
}

// Render writes the compose file and env without starting anything.
func (p *Provider) Render(ctx context.Context, spec provider.Spec) (*provider.Status, error) {
	res, _, err := p.render(ctx, spec)
	if err != nil {
		return nil, err
	}
	return p.status(ctx, spec, res, false)
}

// Up renders and converges the environment.
func (p *Provider) Up(ctx context.Context, spec provider.Spec) (*provider.Status, error) {
	if err := p.requireDocker(); err != nil {
		return nil, err
	}
	res, _, err := p.render(ctx, spec)
	if err != nil {
		return nil, err
	}
	args := []string{"up", "--detach", "--remove-orphans"}
	if spec.Build {
		args = append(args, "--build")
	}
	if spec.Wait {
		args = append(args, "--wait")
	}
	if err := dockerx.Compose(ctx, p.Manifest.Dir, spec.Project, []string{res.File}, nil, args...); err != nil {
		return nil, err
	}
	return p.status(ctx, spec, res, true)
}

// Down stops and removes the environment; volumes optionally.
func (p *Provider) Down(ctx context.Context, spec provider.Spec, volumes bool) error {
	if err := p.requireDocker(); err != nil {
		return err
	}
	file, err := p.renderedFile(ctx, spec)
	if err != nil {
		return err
	}
	args := []string{"down", "--remove-orphans"}
	if volumes {
		args = append(args, "--volumes")
	}
	if err := dockerx.Compose(ctx, p.Manifest.Dir, spec.Project, []string{file}, nil, args...); err != nil {
		return err
	}
	if volumes {
		// Releasing ports only when data is gone keeps a stop/start cycle stable.
		if reg, err := ports.Open(p.RegistryPath, p.Manifest.Ports.Range[0], p.Manifest.Ports.Range[1]); err == nil {
			reg.Release(spec.Project)
			_ = reg.Save()
		}
		return envstate.Remove(p.Manifest.Dir, spec.Env)
	}
	return nil
}

// Stop pauses the environment without removing containers or data.
func (p *Provider) Stop(ctx context.Context, spec provider.Spec) error {
	if err := p.requireDocker(); err != nil {
		return err
	}
	file, err := p.renderedFile(ctx, spec)
	if err != nil {
		return err
	}
	return dockerx.Compose(ctx, p.Manifest.Dir, spec.Project, []string{file}, nil, "stop")
}

// Start resumes a stopped environment.
func (p *Provider) Start(ctx context.Context, spec provider.Spec) error {
	if err := p.requireDocker(); err != nil {
		return err
	}
	file, err := p.renderedFile(ctx, spec)
	if err != nil {
		return err
	}
	return dockerx.Compose(ctx, p.Manifest.Dir, spec.Project, []string{file}, nil, "start")
}

// Status reconciles the rendered files with what Docker reports.
func (p *Provider) Status(ctx context.Context, spec provider.Spec) (*provider.Status, error) {
	if err := p.requireDocker(); err != nil {
		return nil, err
	}
	res, err := p.loadRendered(spec)
	if err != nil {
		return nil, err
	}
	return p.status(ctx, spec, res, true)
}

// Logs streams service logs.
func (p *Provider) Logs(ctx context.Context, spec provider.Spec, follow bool, services ...string) error {
	if err := p.requireDocker(); err != nil {
		return err
	}
	file, err := p.renderedFile(ctx, spec)
	if err != nil {
		return err
	}
	args := []string{"logs"}
	if follow {
		args = append(args, "--follow")
	}
	args = append(args, services...)
	return dockerx.Compose(ctx, p.Manifest.Dir, spec.Project, []string{file}, nil, args...)
}

// Exec runs a command in a service container.
func (p *Provider) Exec(ctx context.Context, spec provider.Spec, service string, args ...string) error {
	if err := p.requireDocker(); err != nil {
		return err
	}
	file, err := p.renderedFile(ctx, spec)
	if err != nil {
		return err
	}
	full := append([]string{"exec", service}, args...)
	return dockerx.Compose(ctx, p.Manifest.Dir, spec.Project, []string{file}, nil, full...)
}

func (p *Provider) renderedFile(ctx context.Context, spec provider.Spec) (string, error) {
	file := filepath.Join(p.OutDir(spec), "compose.yaml")
	if _, err := os.Stat(file); err == nil {
		return file, nil
	}
	// Nothing rendered yet (e.g. `down` on a fresh clone); render so compose
	// can still resolve the project.
	res, _, err := p.render(ctx, spec)
	if err != nil {
		return "", err
	}
	return res.File, nil
}

func (p *Provider) loadRendered(spec provider.Spec) (*compose.Result, error) {
	dir := p.OutDir(spec)
	file := filepath.Join(dir, "compose.yaml")
	if _, err := os.Stat(file); err != nil {
		return nil, fmt.Errorf("environment %s has not been rendered; run `envctl up`", spec.Env)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "env"))
	if err != nil {
		return nil, err
	}
	res := &compose.Result{File: file, EnvFile: filepath.Join(dir, "env")}
	for _, line := range splitLines(string(raw)) {
		if line != "" {
			res.Env = append(res.Env, line)
		}
	}
	return res, nil
}

func (p *Provider) status(ctx context.Context, spec provider.Spec, res *compose.Result, query bool) (*provider.Status, error) {
	st := &provider.Status{
		Name:     spec.Env,
		Feature:  spec.Env,
		Project:  spec.Project,
		Backend:  provider.Local,
		EnvLines: res.Env,
		Rendered: res.File,
	}
	if rec, err := envstate.Load(p.Manifest.Dir, spec.Env); err == nil {
		st.Branch, st.Kept = rec.Branch, rec.Kept
	}
	if !query {
		return st, nil
	}
	entries, err := dockerx.ComposePS(ctx, p.Manifest.Dir, spec.Project, []string{res.File}, nil)
	if err != nil {
		return nil, err
	}
	mode := p.Mode()
	for _, e := range entries {
		st.Services = append(st.Services, provider.Service{Name: e.Service, State: e.State, Health: e.Health})
		if e.State == "running" {
			st.Running = true
		}
		for _, pub := range e.Publishers {
			if pub.PublishedPort == 0 {
				continue
			}
			st.Endpoints = append(st.Endpoints, provider.Endpoint{
				Service: e.Service, Target: pub.TargetPort, Host: "127.0.0.1", Port: pub.PublishedPort,
			})
		}
	}
	for _, x := range p.Manifest.Expose {
		ep := provider.Endpoint{Service: x.Service, Target: x.Port}
		switch mode {
		case compose.ModeDomains:
			ep.Host = compose.DomainHost(x.Service, spec.Project)
			ep.Port = x.Port
			ep.URL = fmt.Sprintf("%s://%s%s", x.Scheme, ep.Host, x.Path)
		default:
			for _, e := range st.Endpoints {
				if e.Service == x.Service && (x.Port == 0 || e.Target == x.Port) {
					ep.Host, ep.Port = e.Host, e.Port
					ep.URL = fmt.Sprintf("%s://%s:%d%s", x.Scheme, e.Host, e.Port, x.Path)
				}
			}
		}
		if ep.URL != "" {
			st.Endpoints = append(st.Endpoints, ep)
		}
	}
	return st, nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
