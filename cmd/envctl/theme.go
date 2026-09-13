package main

import (
	"encoding/json"
	"fmt"
	"os"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/sam-bretz/envctl/internal/tui"
	"github.com/spf13/cobra"
)

func themeCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "theme", Short: "List and choose dashboard themes"}
	c.AddCommand(&cobra.Command{Use: "list", Short: "List built-in themes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, args []string) error {
		path, _ := tui.ConfigPath()
		settings, err := tui.LoadSettings(path)
		if err != nil {
			return err
		}
		current := settings.Theme.Name
		if env := os.Getenv("ENVCTL_THEME"); env != "" {
			current = env
		}
		if current == "" {
			current = tui.Auto
		}
		if g.jsonOut {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"current": current, "config": path, "themes": tui.Themes()})
		}
		// The writer drops color for pipes, NO_COLOR and dumb terminals.
		out := colorprofile.NewWriter(cmd.OutOrStdout(), os.Environ())
		marker := func(name string) string {
			if name == current {
				return "▸"
			}
			return " "
		}
		fmt.Fprintf(out, "%s %-18s follows your terminal: %s / %s\n", marker(tui.Auto), tui.Auto, settings.Theme.Resolve(false).Name, settings.Theme.Resolve(true).Name)
		for _, p := range tui.Themes() {
			mode := "light"
			if p.Dark {
				mode = "dark"
			}
			if p.Name == "terminal" {
				mode = "your terminal's colors"
			}
			swatch := ""
			for _, value := range []string{p.Background, p.Accent, p.Success, p.Warn, p.Danger, p.Info} {
				if value == "" {
					swatch += "  "
					continue
				}
				swatch += lipgloss.NewStyle().Foreground(lipgloss.Color(value)).Render("██")
			}
			fmt.Fprintf(out, "%s %-18s %s %s\n", marker(p.Name), p.Name, swatch, mode)
		}
		fmt.Fprintf(out, "\nSet one with `envctl theme set <name>`, or press T in the dashboard.\n")
		return nil
	}})
	c.AddCommand(&cobra.Command{Use: "set <name>", Short: "Save the dashboard theme to the configuration file", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := (tui.ThemeConfig{Name: args[0]}).Validate(); err != nil {
			return err
		}
		path, err := tui.ConfigPath()
		if err != nil {
			return err
		}
		if err = tui.SaveThemeName(path, args[0]); err != nil {
			return err
		}
		if g.jsonOut {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"theme": args[0], "config": path})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "Theme set to %s in %s\n", args[0], path)
		return err
	}})
	return c
}
