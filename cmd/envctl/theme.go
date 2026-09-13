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
	var forceDark, forceLight bool
	show := &cobra.Command{
		Use:   "show [name]",
		Short: "Explain which dashboard colors will be used and where each comes from",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runThemeShow(cmd, g, args, forceDark, forceLight)
		},
	}
	show.Flags().BoolVar(&forceDark, "dark", false, "assume a dark terminal background (for auto)")
	show.Flags().BoolVar(&forceLight, "light", false, "assume a light terminal background (for auto)")
	show.MarkFlagsMutuallyExclusive("dark", "light")
	c.AddCommand(show)
	return c
}

func runThemeShow(cmd *cobra.Command, g *globals, args []string, forceDark, forceLight bool) error {
	path, err := tui.ConfigPath()
	if err != nil {
		return err
	}
	settings, err := tui.LoadSettings(path)
	if err != nil {
		return err
	}

	name := settings.Theme.Name
	if env := os.Getenv("ENVCTL_THEME"); env != "" {
		name = env
	}
	if len(args) == 1 {
		name = args[0]
	}
	if name == "" {
		name = tui.Auto
	}
	if err := (tui.ThemeConfig{Name: name}).Validate(); err != nil {
		return err
	}

	effective := settings.Theme
	effective.Name = name

	auto := name == tui.Auto
	dark := true
	autoMode := ""
	switch {
	case forceDark:
		dark, autoMode = true, "dark"
	case forceLight:
		dark, autoMode = false, "light"
	case auto:
		dark = lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
		autoMode = "light"
		if dark {
			autoMode = "dark"
		}
	}
	palette, sources := effective.ResolveWithSources(dark)

	if g.jsonOut {
		roles := make(map[string]tui.RoleResolution, len(tui.Roles()))
		for _, role := range tui.Roles() {
			roles[role] = sources[role]
		}
		payload := map[string]any{
			"theme":  palette.Name,
			"auto":   auto,
			"config": path,
			"roles":  roles,
		}
		if auto {
			payload["autoMode"] = autoMode
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(payload)
	}

	out := colorprofile.NewWriter(cmd.OutOrStdout(), os.Environ())
	fmt.Fprintf(out, "Theme:  %s\n", palette.Name)
	if auto {
		fmt.Fprintf(out, "Auto:   resolved to %s (%s)\n", autoMode, palette.Name)
	}
	fmt.Fprintf(out, "Config: %s\n\n", path)
	colored := false
	switch out.Profile {
	case colorprofile.ANSI, colorprofile.ANSI256, colorprofile.TrueColor:
		colored = true
	}
	for _, role := range tui.Roles() {
		r := sources[role]
		swatch := "  "
		if colored && r.Value != "" {
			swatch = lipgloss.NewStyle().Foreground(lipgloss.Color(r.Value)).Render("██")
		}
		value := r.Value
		if value == "" {
			value = "-"
		}
		fmt.Fprintf(out, "%s %-10s %-10s %s\n", swatch, role, value, r.Source)
	}
	return nil
}
