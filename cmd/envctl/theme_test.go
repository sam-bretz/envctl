package main

import (
	"bytes"
	"encoding/json"
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
