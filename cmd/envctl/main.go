// envctl gives every feature branch (and every coding agent's worktree) its own
// isolated copy of a docker-compose stack, locally today and on a per-branch VM
// in a later milestone.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/backend/local"
	"github.com/sam-bretz/envctl/internal/dockerx"
	"github.com/sam-bretz/envctl/internal/envstate"
	"github.com/sam-bretz/envctl/internal/feature"
	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/provider"
)

var version = "dev"

type globals struct {
	dir      string
	env      string
	feat     string // deprecated alias of env
	backend  string
	portMode string
	jsonOut  bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := root().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "envctl:", err)
		os.Exit(1)
	}
}

func root() *cobra.Command {
	g := &globals{}
	cmd := &cobra.Command{
		Use:           "envctl",
		Short:         "Isolated docker-compose environments per feature branch",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := cmd.PersistentFlags()
	pf.StringVarP(&g.dir, "dir", "C", ".", "directory inside the worktree to operate on")
	pf.StringVar(&g.env, "env", "", "environment name (default: the git branch slug)")
	pf.StringVar(&g.feat, "feature", "", "deprecated alias of --env")
	_ = pf.MarkDeprecated("feature", "use --env")
	pf.StringVar(&g.backend, "backend", "local", "backend: local (vm arrives in milestone 3)")
	pf.StringVar(&g.portMode, "port-mode", "", "override ports.mode from the manifest: domains or registry")
	pf.BoolVar(&g.jsonOut, "json", false, "print machine-readable JSON")

	cmd.AddCommand(upCmd(g), downCmd(g), stopCmd(g), startCmd(g), statusCmd(g), renderCmd(g),
		logsCmd(g), execCmd(g), listCmd(g), initCmd(g), hookCmd(g), agentCmd(g), envCmd(g))
	return cmd
}

// resolve loads the manifest, derives the environment and builds the provider.
// The environment name defaults to the branch slug; the linked branch defaults
// to the checked-out branch only for a default-named environment, so an
// explicitly named one stays unlinked unless `env link` says otherwise.
func resolve(ctx context.Context, g *globals) (provider.Provider, provider.Spec, error) {
	m, err := loadManifest(ctx, g.dir)
	if err != nil {
		return nil, provider.Spec{}, err
	}
	if g.portMode != "" {
		switch mode := manifest.PortMode(g.portMode); mode {
		case manifest.PortModeDomains, manifest.PortModeRegistry:
			m.Ports.Mode = mode
		default:
			return nil, provider.Spec{}, fmt.Errorf("--port-mode must be domains or registry, got %q", g.portMode)
		}
	}
	name := g.env
	if name == "" {
		name = g.feat
	}
	spec := provider.Spec{Backend: provider.Backend(g.backend)}
	if name == "" {
		name, err = feature.Detect(ctx, g.dir)
		if err != nil {
			return nil, provider.Spec{}, err
		}
		if b, err := feature.Branch(ctx, g.dir); err == nil && b != "" {
			if _, err := envstate.Load(m.Dir, name); err != nil {
				spec.Branch = b // first sight of a default-named env: link it to its branch
			}
		}
	} else {
		name = feature.Slugify(name)
	}
	spec.Env = name
	spec.Project = feature.ProjectName(m.Project, name)
	switch spec.Backend {
	case provider.Local:
		p, err := local.New(ctx, m)
		if err != nil {
			return nil, spec, err
		}
		return p, spec, nil
	default:
		return nil, spec, fmt.Errorf("backend %q is not implemented yet", g.backend)
	}
}

// loadManifest reads envctl.yaml from the root of the worktree containing dir.
// It deliberately does not walk above the worktree: Claude Code nests
// worktrees under <repo>/.claude/worktrees/, and walking up would pick up the
// parent checkout's manifest and compose files instead of this branch's.
func loadManifest(ctx context.Context, dir string) (*manifest.Manifest, error) {
	if top, err := feature.Toplevel(ctx, dir); err == nil {
		return manifest.Load(top)
	}
	return manifest.Find(dir)
}

func upCmd(g *globals) *cobra.Command {
	var build, noWait bool
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Render and start (or converge) this environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			spec.Build = build
			spec.Wait = !noWait
			st, err := p.Up(cmd.Context(), spec)
			if err != nil {
				return err
			}
			return printStatus(g, st)
		},
	}
	cmd.Flags().BoolVar(&build, "build", false, "rebuild images before starting")
	cmd.Flags().BoolVar(&noWait, "no-wait", false, "do not wait for healthchecks")
	return cmd
}

func downCmd(g *globals) *cobra.Command {
	var volumes bool
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Remove this environment's containers and network (data kept unless --volumes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			return p.Down(cmd.Context(), spec, volumes)
		},
	}
	cmd.Flags().BoolVarP(&volumes, "volumes", "v", false, "also delete volumes and release ports")
	return cmd
}

func stopCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "stop", Short: "Stop containers, keep everything",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			return p.Stop(cmd.Context(), spec)
		},
	}
}

func startCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "start", Short: "Start a stopped environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			return p.Start(cmd.Context(), spec)
		},
	}
}

func statusCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "Show services, endpoints and env for this environment",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			st, err := p.Status(cmd.Context(), spec)
			if err != nil {
				return err
			}
			return printStatus(g, st)
		},
	}
}

func renderCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "render", Short: "Write the isolated compose file and env without starting anything",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			st, err := p.Render(cmd.Context(), spec)
			if err != nil {
				return err
			}
			return printStatus(g, st)
		},
	}
}

func logsCmd(g *globals) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use: "logs [service...]", Short: "Show service logs",
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			return p.Logs(cmd.Context(), spec, follow, args...)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow output")
	return cmd
}

func execCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "exec <service> -- <cmd...>", Short: "Run a command inside a service container",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			return p.Exec(cmd.Context(), spec, args[0], args[1:]...)
		},
	}
}

func listCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List every environment of this project: recorded in this worktree and running on the Docker host",
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadManifest(cmd.Context(), g.dir)
			if err != nil {
				return err
			}
			rows, err := listEnvironments(cmd.Context(), m)
			if err != nil {
				return err
			}
			if g.jsonOut {
				return json.NewEncoder(os.Stdout).Encode(rows)
			}
			fmt.Printf("%-40s %-8s %-30s %-6s %s\n", "NAME", "BACKEND", "BRANCH", "KEPT", "DOCKER")
			for _, r := range rows {
				fmt.Printf("%-40s %-8s %-30s %-6v %s\n", r.Name, r.Backend, r.Branch, r.Kept, r.Docker)
			}
			return nil
		},
	}
}

// envRow is one line of `envctl list` / `envctl env list`.
type envRow struct {
	Name    string `json:"name"`
	Project string `json:"project"`
	Backend string `json:"backend"`
	Branch  string `json:"branch,omitempty"`
	Kept    bool   `json:"kept"`
	Docker  string `json:"docker"` // compose status summary, or "-" if not on this host
}

func listEnvironments(ctx context.Context, m *manifest.Manifest) ([]envRow, error) {
	byName := map[string]*envRow{}
	recorded, err := envstate.List(m.Dir)
	if err != nil {
		return nil, err
	}
	for _, s := range recorded {
		byName[s.Name] = &envRow{Name: s.Name, Project: feature.ProjectName(m.Project, s.Name), Backend: s.Backend, Branch: s.Branch, Kept: s.Kept, Docker: "-"}
	}
	if projects, err := dockerx.ListProjects(ctx, m.Project+"-"); err == nil {
		for _, p := range projects {
			name := strings.TrimPrefix(p.Name, m.Project+"-")
			r, ok := byName[name]
			if !ok {
				r = &envRow{Name: name, Project: p.Name, Backend: "local", Docker: "-"}
				byName[name] = r
			}
			r.Docker = p.Status
		}
	}
	rows := make([]envRow, 0, len(byName))
	for _, r := range byName {
		rows = append(rows, *r)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Name < rows[j].Name })
	return rows, nil
}

func initCmd(g *globals) *cobra.Command {
	var project string
	var files []string
	cmd := &cobra.Command{
		Use: "init", Short: "Write a starter envctl.yaml in this directory",
		RunE: func(cmd *cobra.Command, args []string) error {
			if project == "" {
				return fmt.Errorf("--project is required (short lowercase prefix, e.g. mg)")
			}
			if len(files) == 0 {
				files = []string{"docker-compose.yml"}
			}
			var b strings.Builder
			fmt.Fprintf(&b, "version: 1\nproject: %s\nstack:\n  files:\n", project)
			for _, f := range files {
				fmt.Fprintf(&b, "    - %s\n", f)
			}
			b.WriteString("ports:\n  mode: auto        # auto | domains | registry\n  range: [41000, 49999]\nexpose: []\n")
			path := manifest.FileName
			if g.dir != "." {
				path = g.dir + "/" + manifest.FileName
			}
			if _, err := os.Stat(path); err == nil {
				return fmt.Errorf("%s already exists", path)
			}
			return os.WriteFile(path, []byte(b.String()), 0o644)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "project prefix for compose project names")
	cmd.Flags().StringSliceVar(&files, "file", nil, "compose file(s) relative to the repo root")
	return cmd
}

func hookCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use: "hook claude", Short: "Print the Claude Code hooks that start/stop environments with worktrees",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if args[0] != "claude" {
				return fmt.Errorf("unknown hook target %q", args[0])
			}
			fmt.Print(claudeHooks)
			return nil
		},
	}
}

const claudeHooks = `{
  "hooks": {
    "WorktreeCreate": [
      { "type": "command", "command": "envctl-worktree-create", "timeout": 600 }
    ],
    "WorktreeRemove": [
      { "type": "command", "command": "envctl-worktree-remove", "timeout": 300 }
    ]
  }
}
`

func printStatus(g *globals, st *provider.Status) error {
	if g.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(st)
	}
	branch := st.Branch
	if branch == "" {
		branch = "(unlinked)"
	}
	fmt.Printf("env       %s\nbranch    %s\nproject   %s\nbackend   %s\nrendered  %s\n", st.Name, branch, st.Project, st.Backend, st.Rendered)
	if len(st.Services) > 0 {
		fmt.Println("services")
		for _, s := range st.Services {
			h := s.Health
			if h == "" {
				h = "-"
			}
			fmt.Printf("  %-20s %-10s %s\n", s.Name, s.State, h)
		}
	}
	if len(st.Endpoints) > 0 {
		fmt.Println("endpoints")
		for _, e := range st.Endpoints {
			if e.URL != "" {
				fmt.Printf("  %-20s %s\n", e.Service, e.URL)
			} else {
				fmt.Printf("  %-20s %s:%d -> %d\n", e.Service, e.Host, e.Port, e.Target)
			}
		}
	}
	fmt.Printf("env       source %s\n", strings.TrimSuffix(st.Rendered, "compose.yaml")+"env")
	return nil
}
