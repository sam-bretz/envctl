// Package dockerx wraps the docker CLI: host detection and compose invocation.
// Shelling out keeps behaviour identical to what developers run by hand and
// avoids pinning the engine API; the compose Go API can replace this later.
package dockerx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// Host describes the Docker endpoint envctl is talking to.
type Host struct {
	Context         string
	OperatingSystem string
	OrbStack        bool
}

// DetectHost queries `docker info` and `docker context show`.
func DetectHost(ctx context.Context) (Host, error) {
	var h Host
	out, err := run(ctx, "", nil, "context", "show")
	if err == nil {
		h.Context = strings.TrimSpace(out)
	}
	out, err = run(ctx, "", nil, "info", "--format", "{{.OperatingSystem}}")
	if err != nil {
		return h, fmt.Errorf("docker is not reachable: %w", err)
	}
	h.OperatingSystem = strings.TrimSpace(out)
	h.OrbStack = strings.Contains(h.OperatingSystem, "OrbStack") || h.Context == "orbstack"
	return h, nil
}

// Compose runs `docker compose` with the given project, files and args,
// streaming output to the terminal.
func Compose(ctx context.Context, dir, project string, files []string, env []string, args ...string) error {
	full := composeArgs(project, files, args)
	cmd := exec.CommandContext(ctx, "docker", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// ComposeOutput runs `docker compose` and returns stdout.
func ComposeOutput(ctx context.Context, dir, project string, files []string, env []string, args ...string) (string, error) {
	return run(ctx, dir, env, composeArgs(project, files, args)...)
}

// PSEntry is one row of `docker compose ps --format json`.
type PSEntry struct {
	Name       string `json:"Name"`
	Service    string `json:"Service"`
	State      string `json:"State"`
	Health     string `json:"Health"`
	Publishers []struct {
		URL           string `json:"URL"`
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

// ComposePS lists containers for a project. Compose emits either a JSON array
// or newline-delimited objects depending on version; both are handled.
func ComposePS(ctx context.Context, dir, project string, files []string, env []string) ([]PSEntry, error) {
	out, err := ComposeOutput(ctx, dir, project, files, env, "ps", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	return parsePS(out)
}

func parsePS(out string) ([]PSEntry, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	if strings.HasPrefix(out, "[") {
		var entries []PSEntry
		if err := json.Unmarshal([]byte(out), &entries); err != nil {
			return nil, err
		}
		return entries, nil
	}
	var entries []PSEntry
	dec := json.NewDecoder(strings.NewReader(out))
	for {
		var e PSEntry
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return nil, err
		}
		entries = append(entries, e)
	}
	return entries, nil
}

// ParsePS applies the same Compose status decoder to output collected through
// a guest transport, without querying the developer's Docker daemon.
func ParsePS(out string) ([]PSEntry, error) { return parsePS(out) }

// Project is one row of `docker compose ls --format json`.
type Project struct {
	Name        string `json:"Name"`
	Status      string `json:"Status"`
	ConfigFiles string `json:"ConfigFiles"`
}

// ListProjects returns compose projects whose name starts with prefix.
func ListProjects(ctx context.Context, prefix string) ([]Project, error) {
	out, err := run(ctx, "", nil, "compose", "ls", "--all", "--format", "json")
	if err != nil {
		return nil, err
	}
	var all []Project
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &all); err != nil {
		return nil, err
	}
	var out2 []Project
	for _, p := range all {
		if strings.HasPrefix(p.Name, prefix) {
			out2 = append(out2, p)
		}
	}
	return out2, nil
}

func composeArgs(project string, files []string, args []string) []string {
	full := []string{"compose", "--project-name", project}
	for _, f := range files {
		full = append(full, "--file", f)
	}
	return append(full, args...)
}

func run(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = append(os.Environ(), env...)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("docker %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}
