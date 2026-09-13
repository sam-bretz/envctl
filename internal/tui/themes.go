package tui

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Palette is a complete set of dashboard colors. A value is "#rrggbb", an ANSI
// index "0"-"255" (which follows the terminal's own palette), or "" for the
// terminal default.
type Palette struct {
	Name       string `json:"name"`
	Dark       bool   `json:"dark"`
	Background string `json:"background"`
	Surface    string `json:"surface"`
	Text       string `json:"text"`
	Muted      string `json:"muted"`
	Line       string `json:"line"`
	Accent     string `json:"accent"`
	Success    string `json:"success"`
	Danger     string `json:"danger"`
	Warn       string `json:"warn"`
	Info       string `json:"info"`
}

// Auto picks the light or dark theme from the terminal's reported background.
const Auto = "auto"

const defaultDark, defaultLight = "catppuccin", "catppuccin-latte"

var builtins = []Palette{
	{Name: "terminal", Dark: true, Muted: "8", Line: "8", Accent: "4", Success: "2", Danger: "1", Warn: "3", Info: "5"},
	{Name: "catppuccin", Dark: true, Background: "#1e1e2e", Surface: "#313244", Text: "#cdd6f4", Muted: "#7f849c", Line: "#45475a", Accent: "#89b4fa", Success: "#a6e3a1", Danger: "#f38ba8", Warn: "#f9e2af", Info: "#cba6f7"},
	{Name: "catppuccin-latte", Background: "#eff1f5", Surface: "#ccd0da", Text: "#4c4f69", Muted: "#8c8fa1", Line: "#bcc0cc", Accent: "#1e66f5", Success: "#40a02b", Danger: "#d20f39", Warn: "#df8e1d", Info: "#8839ef"},
	{Name: "tokyo-night", Dark: true, Background: "#1a1b26", Surface: "#292e42", Text: "#c0caf5", Muted: "#565f89", Line: "#3b4261", Accent: "#7aa2f7", Success: "#9ece6a", Danger: "#f7768e", Warn: "#e0af68", Info: "#bb9af7"},
	{Name: "tokyo-night-day", Background: "#e1e2e7", Surface: "#c4c8da", Text: "#3760bf", Muted: "#848cb5", Line: "#a8aecb", Accent: "#2e7de9", Success: "#587539", Danger: "#f52a65", Warn: "#8c6c3e", Info: "#9854f1"},
	{Name: "gruvbox", Dark: true, Background: "#282828", Surface: "#3c3836", Text: "#ebdbb2", Muted: "#928374", Line: "#504945", Accent: "#83a598", Success: "#b8bb26", Danger: "#fb4934", Warn: "#fabd2f", Info: "#d3869b"},
	{Name: "gruvbox-light", Background: "#fbf1c7", Surface: "#ebdbb2", Text: "#3c3836", Muted: "#928374", Line: "#d5c4a1", Accent: "#076678", Success: "#79740e", Danger: "#9d0006", Warn: "#b57614", Info: "#8f3f71"},
	{Name: "nord", Dark: true, Background: "#2e3440", Surface: "#3b4252", Text: "#d8dee9", Muted: "#616e88", Line: "#4c566a", Accent: "#88c0d0", Success: "#a3be8c", Danger: "#bf616a", Warn: "#ebcb8b", Info: "#b48ead"},
	{Name: "dracula", Dark: true, Background: "#282a36", Surface: "#44475a", Text: "#f8f8f2", Muted: "#6272a4", Line: "#44475a", Accent: "#bd93f9", Success: "#50fa7b", Danger: "#ff5555", Warn: "#f1fa8c", Info: "#8be9fd"},
	{Name: "rose-pine", Dark: true, Background: "#191724", Surface: "#26233a", Text: "#e0def4", Muted: "#6e6a86", Line: "#403d52", Accent: "#c4a7e7", Success: "#9ccfd8", Danger: "#eb6f92", Warn: "#f6c177", Info: "#ebbcba"},
	{Name: "rose-pine-dawn", Background: "#faf4ed", Surface: "#f2e9e1", Text: "#575279", Muted: "#9893a5", Line: "#dfdad9", Accent: "#907aa9", Success: "#286983", Danger: "#b4637a", Warn: "#ea9d34", Info: "#d7827e"},
	{Name: "kanagawa", Dark: true, Background: "#1f1f28", Surface: "#2a2a37", Text: "#dcd7ba", Muted: "#727169", Line: "#363646", Accent: "#7e9cd8", Success: "#98bb6c", Danger: "#e46876", Warn: "#e6c384", Info: "#957fb8"},
	{Name: "everforest", Dark: true, Background: "#2d353b", Surface: "#3d484d", Text: "#d3c6aa", Muted: "#859289", Line: "#475258", Accent: "#7fbbb3", Success: "#a7c080", Danger: "#e67e80", Warn: "#dbbc7f", Info: "#d699b6"},
	{Name: "solarized-dark", Dark: true, Background: "#002b36", Surface: "#073642", Text: "#839496", Muted: "#657b83", Line: "#586e75", Accent: "#268bd2", Success: "#859900", Danger: "#dc322f", Warn: "#b58900", Info: "#2aa198"},
	{Name: "solarized-light", Background: "#fdf6e3", Surface: "#eee8d5", Text: "#657b83", Muted: "#839496", Line: "#93a1a1", Accent: "#268bd2", Success: "#859900", Danger: "#dc322f", Warn: "#b58900", Info: "#2aa198"},
	{Name: "github-dark", Dark: true, Background: "#0d1117", Surface: "#21262d", Text: "#e6edf3", Muted: "#8b949e", Line: "#30363d", Accent: "#58a6ff", Success: "#3fb950", Danger: "#f85149", Warn: "#d29922", Info: "#bc8cff"},
	{Name: "github-light", Background: "#ffffff", Surface: "#eaeef2", Text: "#1f2328", Muted: "#656d76", Line: "#d0d7de", Accent: "#0969da", Success: "#1a7f37", Danger: "#cf222e", Warn: "#9a6700", Info: "#8250df"},
}

// Themes lists the built-in palettes in display order.
func Themes() []Palette { return slices.Clone(builtins) }

// Lookup finds a built-in palette by name.
func Lookup(name string) (Palette, bool) {
	for _, p := range builtins {
		if p.Name == name {
			return p, true
		}
	}
	return Palette{}, false
}

// ThemeConfig is the `theme` section of the user configuration file.
type ThemeConfig struct {
	// Name is a built-in theme, or "auto" (the default) to follow the
	// terminal's light or dark background.
	Name  string `yaml:"name,omitempty" json:"name,omitempty"`
	Light string `yaml:"light,omitempty" json:"light,omitempty"`
	Dark  string `yaml:"dark,omitempty" json:"dark,omitempty"`
	// Background paints the theme's background. False keeps the terminal's own,
	// for example a transparent or image background.
	Background *bool `yaml:"background,omitempty" json:"background,omitempty"`
	// Custom overrides individual palette roles of the selected theme.
	Custom map[string]string `yaml:"custom,omitempty" json:"custom,omitempty"`
}

// Settings is the user configuration file, ~/.config/envctl/config.yaml.
type Settings struct {
	Theme ThemeConfig `yaml:"theme,omitempty" json:"theme"`
}

var customRoles = []string{"background", "surface", "text", "muted", "line", "accent", "success", "danger", "warn", "info"}
var hexColor = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{6})$`)

// ThemeNames lists every value accepted by theme.name.
func ThemeNames() []string {
	names := []string{Auto}
	for _, p := range builtins {
		names = append(names, p.Name)
	}
	return names
}

func validColor(value string) bool {
	if value == "" || value == "none" || value == "default" || hexColor.MatchString(value) {
		return true
	}
	i, err := strconv.Atoi(value)
	return err == nil && i >= 0 && i <= 255 && strconv.Itoa(i) == value
}

func knownTheme(name string, allowAuto bool) error {
	if name == "" || (allowAuto && name == Auto) {
		return nil
	}
	if _, ok := Lookup(name); !ok {
		return fmt.Errorf("unknown theme %q; choose one of: %s", name, strings.Join(ThemeNames(), ", "))
	}
	return nil
}

// Validate rejects unknown themes, roles and color values.
func (c ThemeConfig) Validate() error {
	if err := knownTheme(c.Name, true); err != nil {
		return err
	}
	for _, name := range []string{c.Light, c.Dark} {
		if err := knownTheme(name, false); err != nil {
			return err
		}
	}
	for role, value := range c.Custom {
		if !slices.Contains(customRoles, role) {
			return fmt.Errorf("theme.custom.%s is not a palette role; use one of: %s", role, strings.Join(customRoles, ", "))
		}
		if !validColor(value) {
			return fmt.Errorf("theme.custom.%s: %q is not a color; use #rrggbb, an ANSI index 0-255, or none", role, value)
		}
	}
	return nil
}

// Resolve returns the palette to draw with. dark is the terminal's reported
// background; it only matters for "auto".
func (c ThemeConfig) Resolve(dark bool) Palette {
	p, _ := c.ResolveWithSources(dark)
	return p
}

// RoleResolution is one palette role's resolved value and where it came from.
type RoleResolution struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

// Roles lists the palette roles theme.custom.<role> and `envctl theme show`
// report, in display order.
func Roles() []string { return slices.Clone(customRoles) }

// ResolveWithSources is Resolve, plus where every role's value came from: the
// built-in theme's name, "theme.custom.<role>", "disabled by theme.background:
// false", or "terminal default" for an empty value. The dashboard and
// `envctl theme show` both resolve through this function so they cannot
// diverge.
func (c ThemeConfig) ResolveWithSources(dark bool) (Palette, map[string]RoleResolution) {
	name := c.Name
	if name == "" || name == Auto {
		name = defaultLight
		if c.Light != "" {
			name = c.Light
		}
		if dark {
			name = defaultDark
			if c.Dark != "" {
				name = c.Dark
			}
		}
	}
	p, ok := Lookup(name)
	if !ok {
		p, _ = Lookup(defaultDark)
	}
	sources := make(map[string]RoleResolution, len(customRoles))
	for _, role := range customRoles {
		field := roleField(&p, role)
		source := p.Name
		if role == "background" && c.Background != nil && !*c.Background {
			*field = ""
			source = "disabled by theme.background: false"
		}
		if value, ok := c.Custom[role]; ok {
			if value == "none" || value == "default" {
				value = ""
			}
			*field = value
			if value == "" {
				source = "terminal default"
			} else {
				source = "theme.custom." + role
			}
		}
		if *field == "" && source == p.Name {
			source = "terminal default"
		}
		sources[role] = RoleResolution{Value: *field, Source: source}
	}
	return p, sources
}

// roleField returns a pointer to p's field for role, one of customRoles.
func roleField(p *Palette, role string) *string {
	switch role {
	case "background":
		return &p.Background
	case "surface":
		return &p.Surface
	case "text":
		return &p.Text
	case "muted":
		return &p.Muted
	case "line":
		return &p.Line
	case "accent":
		return &p.Accent
	case "success":
		return &p.Success
	case "danger":
		return &p.Danger
	case "warn":
		return &p.Warn
	case "info":
		return &p.Info
	}
	panic("unknown role " + role)
}

// ConfigPath is $XDG_CONFIG_HOME/envctl/config.yaml, or ~/.config/envctl/config.yaml.
func ConfigPath() (string, error) {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "envctl", "config.yaml"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "envctl", "config.yaml"), nil
}

// LoadSettings reads the user configuration. A missing file is empty settings.
func LoadSettings(path string) (Settings, error) {
	var s Settings
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err = yaml.Unmarshal(raw, &s); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	if err = s.Theme.Validate(); err != nil {
		return s, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// SaveThemeName sets theme.name in the configuration file, keeping every other
// key and comment. The write is atomic and private.
func SaveThemeName(path, name string) error {
	if err := knownTheme(name, true); err != nil || name == "" {
		return errors.Join(err, errors.New("a theme name is required"))
	}
	var doc yaml.Node
	raw, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		raw = nil
	case err != nil:
		return err
	}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err = yaml.Unmarshal(raw, &doc); err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
	}
	if doc.Kind == 0 {
		doc = yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{{Kind: yaml.MappingNode}}}
	}
	root := doc.Content[0]
	if root.Kind != yaml.MappingNode {
		return fmt.Errorf("%s: the configuration must be a mapping", path)
	}
	section := mappingValue(root, "theme")
	if section.Kind != yaml.MappingNode {
		*section = yaml.Node{Kind: yaml.MappingNode}
	}
	*mappingValue(section, "name") = yaml.Node{Kind: yaml.ScalarNode, Value: name}
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err = encoder.Encode(&doc); err != nil {
		return err
	}
	if err = encoder.Close(); err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".config-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(out.Bytes()); err != nil {
		f.Close()
		return err
	}
	if err = f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// mappingValue returns the value node for key, adding the key when absent.
func mappingValue(m *yaml.Node, key string) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	value := &yaml.Node{Kind: yaml.MappingNode}
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: key}, value)
	return value
}
