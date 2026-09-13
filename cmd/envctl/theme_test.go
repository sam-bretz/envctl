package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/tui"
)

func TestThemeCommandsSaveAndList(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ENVCTL_THEME", "")
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := root()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	if _, err := run("theme", "set", "not-a-theme"); err == nil {
		t.Fatal("saved an unknown theme")
	}
	if _, err := run("theme", "set", "gruvbox"); err != nil {
		t.Fatal(err)
	}
	out, err := run("--json", "theme", "list")
	if err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Current string        `json:"current"`
		Themes  []tui.Palette `json:"themes"`
	}
	if json.Unmarshal([]byte(out), &listed) != nil || listed.Current != "gruvbox" || len(listed.Themes) != len(tui.Themes()) {
		t.Fatalf("theme list did not report the saved theme: %s", out)
	}
	if out, err = run("theme", "list"); err != nil || !strings.Contains(out, "▸ gruvbox") || strings.ContainsRune(out, '\x1b') {
		t.Fatalf("human listing unmarked or colored for a non-terminal: %q", out)
	}
}

func TestThemeShow(t *testing.T) {
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		cmd := root()
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		err := cmd.Execute()
		return out.String(), err
	}
	type shown struct {
		Theme    string                        `json:"theme"`
		Auto     bool                          `json:"auto"`
		AutoMode string                        `json:"autoMode"`
		Config   string                        `json:"config"`
		Roles    map[string]tui.RoleResolution `json:"roles"`
	}

	t.Run("no-arg precedence", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("ENVCTL_THEME", "")
		if _, err := run("theme", "set", "nord"); err != nil {
			t.Fatal(err)
		}
		out, err := run("--json", "theme", "show")
		if err != nil {
			t.Fatal(err)
		}
		var s shown
		if json.Unmarshal([]byte(out), &s) != nil || s.Theme != "nord" {
			t.Fatalf("expected the config file's theme, got %s", out)
		}

		t.Setenv("ENVCTL_THEME", "dracula")
		if out, err = run("--json", "theme", "show"); err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal([]byte(out), &s) != nil || s.Theme != "dracula" {
			t.Fatalf("ENVCTL_THEME did not override the config file: %s", out)
		}

		t.Setenv("ENVCTL_THEME", "")
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		if out, err = run("--json", "theme", "show"); err != nil {
			t.Fatal(err)
		}
		if json.Unmarshal([]byte(out), &s) != nil || !s.Auto {
			t.Fatalf("expected auto with no env or config file: %s", out)
		}
	})

	t.Run("explicit name argument with file overrides", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		t.Setenv("ENVCTL_THEME", "")
		path := filepath.Join(dir, "envctl", "config.yaml")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		yaml := "theme:\n  background: false\n  custom:\n    accent: \"#ff0000\"\n"
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := run("--json", "theme", "show", "nord")
		if err != nil {
			t.Fatal(err)
		}
		var s shown
		if json.Unmarshal([]byte(out), &s) != nil || s.Theme != "nord" {
			t.Fatalf("expected the named theme, got %s", out)
		}
		if r := s.Roles["background"]; r.Source != "disabled by theme.background: false" {
			t.Fatalf("background = %+v, want disabled", r)
		}
		if r := s.Roles["accent"]; r.Value != "#ff0000" || r.Source != "theme.custom.accent" {
			t.Fatalf("accent = %+v, want the custom override", r)
		}
	})

	t.Run("--dark and --light force auto", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("ENVCTL_THEME", "")
		out, err := run("--json", "theme", "show", "--dark")
		if err != nil {
			t.Fatal(err)
		}
		var dark shown
		if json.Unmarshal([]byte(out), &dark) != nil || dark.AutoMode != "dark" || dark.Theme != "catppuccin" {
			t.Fatalf("--dark did not force the dark theme: %s", out)
		}
		out, err = run("--json", "theme", "show", "--light")
		if err != nil {
			t.Fatal(err)
		}
		var light shown
		if json.Unmarshal([]byte(out), &light) != nil || light.AutoMode != "light" || light.Theme != "catppuccin-latte" {
			t.Fatalf("--light did not force the light theme: %s", out)
		}
		if _, err = run("theme", "show", "--dark", "--light"); err == nil {
			t.Fatal("--dark and --light together were accepted")
		}
	})

	t.Run("json shape", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("ENVCTL_THEME", "")
		out, err := run("--json", "theme", "show", "--dark")
		if err != nil {
			t.Fatal(err)
		}
		var s shown
		if json.Unmarshal([]byte(out), &s) != nil {
			t.Fatalf("could not unmarshal: %s", out)
		}
		if len(s.Roles) != len(tui.Roles()) {
			t.Fatalf("got %d roles, want %d", len(s.Roles), len(tui.Roles()))
		}
		for role, r := range s.Roles {
			if r.Source == "" {
				t.Fatalf("%s has no source", role)
			}
		}
	})

	t.Run("unknown name matches theme set", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv("ENVCTL_THEME", "")
		_, errShow := run("theme", "show", "not-a-theme")
		_, errSet := run("theme", "set", "not-a-theme")
		if errShow == nil || errSet == nil || errShow.Error() != errSet.Error() {
			t.Fatalf("errors differ: show=%v set=%v", errShow, errSet)
		}
	})
}

func TestDashboardThemeOverrides(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("ENVCTL_THEME", "nord")
	opts, err := dashboardOptions("")
	if err != nil || opts.Theme != "nord" {
		t.Fatal("ENVCTL_THEME not applied", opts, err)
	}
	if opts, err = dashboardOptions("dracula"); err != nil || opts.Theme != "dracula" {
		t.Fatal("--theme did not take precedence", opts, err)
	}
	t.Setenv("ENVCTL_THEME", "neon")
	if _, err = dashboardOptions(""); err == nil {
		t.Fatal("an unknown theme was accepted")
	}
}
