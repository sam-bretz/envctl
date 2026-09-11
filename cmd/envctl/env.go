package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/envstate"
	"github.com/sam-bretz/envctl/internal/feature"
	"github.com/sam-bretz/envctl/internal/provider"
)

// envCmd manages environment identity: named environments that outlive a
// branch, can be linked to a different branch, or unlinked and kept.
func envCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "env",
		Short: "Create, link, unlink, list and remove named environments",
	}
	cmd.AddCommand(envCreateCmd(g), envLinkCmd(g), envUnlinkCmd(g), envListCmd(g), envRmCmd(g))
	return cmd
}

func envCreateCmd(g *globals) *cobra.Command {
	var branch string
	var noUp bool
	cmd := &cobra.Command{
		Use:   "create <name>",
		Short: "Create a named, kept environment (optionally linked to a branch) and start it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g.env = args[0]
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			spec.Kept = true
			spec.Branch = branch
			m, err := loadManifest(cmd.Context(), g.dir)
			if err != nil {
				return err
			}
			if _, err := envstate.Upsert(m.Dir, spec.Env, string(spec.Backend), func(s *envstate.State) {
				s.Kept = true
				if branch != "" {
					s.Branch = branch
				}
			}); err != nil {
				return err
			}
			if noUp {
				st, err := p.Render(cmd.Context(), spec)
				if err != nil {
					return err
				}
				return printStatus(g, st)
			}
			spec.Wait = true
			st, err := p.Up(cmd.Context(), spec)
			if err != nil {
				return err
			}
			return printStatus(g, st)
		},
	}
	cmd.Flags().StringVar(&branch, "branch", "", "branch this environment follows (default: none)")
	cmd.Flags().BoolVar(&noUp, "no-up", false, "record and render only; do not start containers")
	return cmd
}

func envLinkCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "link <name> <branch>",
		Short: "Point an environment at a branch; CI updates it on every push to that branch",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadManifest(cmd.Context(), g.dir)
			if err != nil {
				return err
			}
			name := feature.Slugify(args[0])
			s, err := envstate.Upsert(m.Dir, name, string(provider.Local), func(s *envstate.State) { s.Branch = args[1] })
			if err != nil {
				return err
			}
			return printState(g, s)
		},
	}
}

func envUnlinkCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "unlink <name>",
		Short: "Stop an environment following any branch; it is kept until `env rm`",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := loadManifest(cmd.Context(), g.dir)
			if err != nil {
				return err
			}
			name := feature.Slugify(args[0])
			if _, err := envstate.Load(m.Dir, name); err != nil {
				return fmt.Errorf("environment %s is not recorded in this worktree", name)
			}
			s, err := envstate.Upsert(m.Dir, name, string(provider.Local), func(s *envstate.State) {
				s.Branch = ""
				s.Kept = true
			})
			if err != nil {
				return err
			}
			return printState(g, s)
		},
	}
}

func envListCmd(g *globals) *cobra.Command {
	c := listCmd(g)
	c.Use = "list"
	return c
}

func envRmCmd(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "rm <name>",
		Short: "Destroy an environment: containers, volumes, ports and its record",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			g.env = args[0]
			p, spec, err := resolve(cmd.Context(), g)
			if err != nil {
				return err
			}
			return p.Down(cmd.Context(), spec, true)
		},
	}
}

func printState(g *globals, s *envstate.State) error {
	if g.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(s)
	}
	branch := s.Branch
	if branch == "" {
		branch = "(unlinked)"
	}
	fmt.Printf("env       %s\nbranch    %s\nbackend   %s\nkept      %v\n", s.Name, branch, s.Backend, s.Kept)
	return nil
}
