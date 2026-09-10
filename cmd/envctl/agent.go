package main

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/feature"
	"github.com/sam-bretz/envctl/skills"
)

// agentCmd installs the envctl skill where coding agents look for skills and
// prints the paragraph to add to CLAUDE.md or AGENTS.md.
func agentCmd(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Teach coding agents (Claude Code, Codex, others) how to use envctl",
	}
	cmd.AddCommand(agentInstallCmd(g), agentSnippetCmd())
	return cmd
}

func agentInstallCmd(g *globals) *cobra.Command {
	var global bool
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Copy the envctl skill into .claude/skills and .agents/skills (or the user-level equivalents with --global)",
		RunE: func(cmd *cobra.Command, args []string) error {
			targets, err := skillTargets(g.dir, global)
			if err != nil {
				return err
			}
			for _, t := range targets {
				if err := writeSkill(t); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), "installed", filepath.Join(t, "SKILL.md"))
			}
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprintln(cmd.OutOrStdout(), "Add this to CLAUDE.md and AGENTS.md so agents know the environment exists:")
			fmt.Fprintln(cmd.OutOrStdout())
			fmt.Fprint(cmd.OutOrStdout(), agentSnippet)
			return nil
		},
	}
	cmd.Flags().BoolVar(&global, "global", false, "install for the user (~/.claude, ~/.agents, ~/.codex) instead of this repository")
	return cmd
}

func agentSnippetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "snippet",
		Short: "Print the CLAUDE.md / AGENTS.md paragraph",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), agentSnippet)
			return nil
		},
	}
}

// skillTargets returns the directories the skill is copied into. Claude Code
// reads .claude/skills; Codex and the cross-agent convention read
// .agents/skills; Codex also reads ~/.codex/skills for user-level skills.
func skillTargets(dir string, global bool) ([]string, error) {
	if global {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		return []string{
			filepath.Join(home, ".claude", "skills", skills.Name),
			filepath.Join(home, ".agents", "skills", skills.Name),
			filepath.Join(home, ".codex", "skills", skills.Name),
		}, nil
	}
	root, err := feature.Toplevel(context.Background(), dir)
	if err != nil {
		root, err = filepath.Abs(dir)
		if err != nil {
			return nil, err
		}
	}
	return []string{
		filepath.Join(root, ".claude", "skills", skills.Name),
		filepath.Join(root, ".agents", "skills", skills.Name),
	}, nil
}

func writeSkill(dst string) error {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(skills.FS, skills.Name, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := skills.FS.ReadFile(path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(path, skills.Name+"/")
		return os.WriteFile(filepath.Join(dst, rel), raw, 0o644)
	})
}

const agentSnippet = `## Local services (envctl)

This repository's docker-compose stack is managed by envctl. Every git
worktree has its own isolated environment. Run ` + "`envctl up`" + ` to start it
and ` + "`envctl status --json`" + ` to find service hosts and ports. Never run
` + "`docker compose`" + ` directly on the repo's compose files and never assume a
fixed port such as localhost:5433. See the envctl skill for details.
`
