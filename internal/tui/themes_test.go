package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

func TestBuiltinPalettesAreCompleteAndValid(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range Themes() {
		if p.Name == "" || p.Name == Auto || seen[p.Name] {
			t.Fatalf("invalid or duplicate theme name %q", p.Name)
		}
		seen[p.Name] = true
		for role, value := range map[string]string{"background": p.Background, "surface": p.Surface, "text": p.Text, "muted": p.Muted, "line": p.Line, "accent": p.Accent, "success": p.Success, "danger": p.Danger, "warn": p.Warn, "info": p.Info} {
			if !validColor(value) {
				t.Fatalf("%s.%s: invalid color %q", p.Name, role, value)
			}
			if p.Name != "terminal" && value == "" {
				t.Fatalf("%s.%s: a painted theme must define every role", p.Name, role)
			}
		}
	}
	if p, _ := Lookup("terminal"); p.Background != "" {
		t.Fatal("the terminal theme must keep the terminal's own background")
	}
}

func TestThemeResolution(t *testing.T) {
	var c ThemeConfig
	if c.Resolve(true).Name != defaultDark || c.Resolve(false).Name != defaultLight {
		t.Fatal("auto did not follow the terminal background")
	}
	c = ThemeConfig{Name: Auto, Dark: "nord", Light: "gruvbox-light"}
	if c.Resolve(true).Name != "nord" || c.Resolve(false).Name != "gruvbox-light" {
		t.Fatal("auto ignored the configured light and dark themes")
	}
	off := false
	c = ThemeConfig{Name: "dracula", Background: &off, Custom: map[string]string{"accent": "#ff9e64", "surface": "none"}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	p := c.Resolve(false)
	if p.Name != "dracula" || p.Background != "" || p.Accent != "#ff9e64" || p.Surface != "" || p.Success != "#50fa7b" {
		t.Fatalf("overrides not applied: %+v", p)
	}
	for _, bad := range []ThemeConfig{
		{Name: "solarised"},
		{Dark: Auto},
		{Custom: map[string]string{"sidebar": "#000000"}},
		{Custom: map[string]string{"accent": "blue"}},
		{Custom: map[string]string{"accent": "256"}},
	} {
		if bad.Validate() == nil {
			t.Fatalf("accepted invalid theme configuration %+v", bad)
		}
	}
}

func TestSaveThemeNameKeepsTheRestOfTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "config.yaml")
	if err := SaveThemeName(path, "kanagawa"); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadSettings(path); err != nil || s.Theme.Name != "kanagawa" {
		t.Fatal("new configuration not written", s, err)
	}
	if err := os.WriteFile(path, []byte("# mine\ntheme:\n  dark: nord # night\n  name: gruvbox\nother: keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveThemeName(path, "everforest"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(path)
	text := string(raw)
	for _, want := range []string{"# mine", "dark: nord # night", "name: everforest", "other: keep"} {
		if !strings.Contains(text, want) {
			t.Fatalf("saved file lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "gruvbox") {
		t.Fatal("old theme name kept")
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Fatalf("configuration mode %v, want 0600", info.Mode().Perm())
	}
	if SaveThemeName(path, "nope") == nil || SaveThemeName(path, "") == nil {
		t.Fatal("saved an invalid theme name")
	}
	if err := os.WriteFile(path, []byte("theme:\n  name: nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSettings(path); err == nil {
		t.Fatal("loaded an invalid configuration")
	}
}

func TestPickerPreviewsRestoresAndSaves(t *testing.T) {
	m := styledFixture(t, colorprofile.TrueColor, 2)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	m.ConfigPath = filepath.Join(t.TempDir(), "config.yaml")
	m.Theme.config = ThemeConfig{Name: "nord"}
	m.Theme.resolve()

	m, _ = key(m, "T")
	if m.Picker == nil || m.Picker.names[m.Picker.index] != "nord" {
		t.Fatal("picker did not open on the configured theme")
	}
	m, _ = key(m, "j")
	previewed := m.Theme.name()
	if previewed == "nord" {
		t.Fatal("moving the selection did not preview another theme")
	}
	if view := m.View().Content; !strings.Contains(ansi.Strip(view), "Theme") || !strings.Contains(ansi.Strip(view), "▸ "+previewed) {
		t.Fatal("picker is not drawn with the previewed theme selected")
	}
	if !strings.Contains(ansi.Strip(m.View().Content), "follows your terminal: "+defaultLight+" / "+defaultDark) {
		t.Fatal("auto is not described by the themes it would choose")
	}
	m, _ = key(m, "esc")
	if m.Picker != nil || m.Theme.name() != "nord" {
		t.Fatal("escape did not restore the original theme")
	}
	if _, err := os.Stat(m.ConfigPath); err == nil {
		t.Fatal("escape saved a theme")
	}

	m, _ = key(m, "T")
	m, _ = key(m, "j")
	chosen := m.Picker.names[m.Picker.index]
	m, _ = key(m, "enter")
	if m.Picker != nil || m.Theme.name() != chosen || m.Error != "" {
		t.Fatal("enter did not apply the theme", m.Error)
	}
	if s, err := LoadSettings(m.ConfigPath); err != nil || s.Theme.Name != chosen {
		t.Fatal("enter did not save the theme", s, err)
	}
	if m.API.(*fakeAPI).request.Action != "" {
		t.Fatal("theme keys reached the workflow API")
	}
}

func TestEveryThemeFitsAndPaintsTheScreen(t *testing.T) {
	for _, name := range ThemeNames() {
		m := styledFixture(t, colorprofile.TrueColor, 4)
		m.Theme.config = ThemeConfig{Name: name}
		next, _ := m.Update(tea.BackgroundColorMsg{})
		m = next.(Model)
		for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 40}, {Width: 55, Height: 18}, {Width: 20, Height: 8}} {
			next, _ := m.Update(size)
			view := next.(Model).View().Content
			lines := strings.Split(view, "\n")
			if len(lines) > size.Height {
				t.Fatalf("%s %dx%d: %d lines", name, size.Width, size.Height, len(lines))
			}
			for _, line := range lines {
				if ansi.StringWidth(line) > size.Width {
					t.Fatalf("%s %dx%d: line too wide: %q", name, size.Width, size.Height, ansi.Strip(line))
				}
			}
			if m.Theme.pal.Background != "" && len(lines) != size.Height {
				t.Fatalf("%s: painted theme left %d rows unpainted", name, size.Height-len(lines))
			}
		}
	}
	plain := styledFixture(t, colorprofile.Ascii, 2)
	plain.Theme.config = ThemeConfig{Name: "dracula"}
	plain.Theme.resolve()
	if strings.ContainsRune(plain.View().Content, '\x1b') {
		t.Fatal("a theme emitted escapes on a terminal without color")
	}
}
