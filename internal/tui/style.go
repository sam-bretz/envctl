package tui

import (
	"image/color"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
)

// theme resolves the palette from what the terminal actually reports. Its zero
// value renders plain text: a pipe, a dumb terminal, NO_COLOR and the tests all
// get layout without a single escape sequence, so sanitized content stays the
// only thing the client can emit.
type theme struct {
	profile colorprofile.Profile
	dark    bool
	// config is the user's theme section; override is --theme or ENVCTL_THEME;
	// preview is the picker's highlighted theme while it is open.
	config   ThemeConfig
	override string
	preview  string
	pal      Palette
}

// resolve recomputes the palette after any input to the choice changes.
func (t *theme) resolve() {
	c := t.config
	if t.override != "" {
		c.Name = t.override
	}
	if t.preview != "" {
		c.Name = t.preview
	}
	t.pal = c.Resolve(t.dark)
}

// name is the theme currently drawn, "auto" resolved to its concrete palette.
func (t theme) name() string { return t.pal.Name }

func (t theme) colored() bool {
	switch t.profile {
	case colorprofile.ANSI, colorprofile.ANSI256, colorprofile.TrueColor:
		return true
	}
	return false
}

// color converts a palette value; "" is the terminal default and returns nil.
func paletteColor(value string) color.Color {
	if value == "" {
		return nil
	}
	return lipgloss.Color(value)
}

func (t theme) accent() color.Color  { return paletteColor(t.pal.Accent) }
func (t theme) success() color.Color { return paletteColor(t.pal.Success) }
func (t theme) danger() color.Color  { return paletteColor(t.pal.Danger) }
func (t theme) warn() color.Color    { return paletteColor(t.pal.Warn) }
func (t theme) muted() color.Color   { return paletteColor(t.pal.Muted) }
func (t theme) line() color.Color    { return paletteColor(t.pal.Line) }
func (t theme) info() color.Color    { return paletteColor(t.pal.Info) }

// style returns the base text style: the palette's text color on a color
// terminal, nothing otherwise.
func (t theme) style() lipgloss.Style { return t.fg(paletteColor(t.pal.Text)) }

func (t theme) fg(c color.Color) lipgloss.Style {
	if !t.colored() || c == nil {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(c)
}
func (t theme) strong(c color.Color) lipgloss.Style {
	if !t.colored() {
		return lipgloss.NewStyle()
	}
	return t.fg(c).Bold(true)
}
func (t theme) bold() lipgloss.Style {
	if !t.colored() {
		return lipgloss.NewStyle()
	}
	return t.style().Bold(true)
}
func (t theme) dim() lipgloss.Style { return t.fg(t.muted()) }

// selected marks the focused row without relying on color alone: the caret in
// the row text carries the same meaning on a monochrome terminal.
func (t theme) selected() lipgloss.Style {
	if !t.colored() {
		return lipgloss.NewStyle()
	}
	return t.strong(t.accent())
}

// paint fills an already-styled segment with a background color. Every style
// reset inside it would otherwise clear the background for the rest of the
// segment, so each reset re-applies it.
func (t theme) paint(segment, background string) string {
	if !t.colored() || background == "" {
		return segment
	}
	on := ansi.Style{}.BackgroundColor(paletteColor(background)).String()
	segment = strings.ReplaceAll(segment, "\x1b[0m", ansi.ResetStyle)
	return on + strings.ReplaceAll(segment, ansi.ResetStyle, ansi.ResetStyle+on) + ansi.ResetStyle
}

// highlight paints the selection surface behind a segment.
func (t theme) highlight(segment string) string { return t.paint(segment, t.pal.Surface) }

// canvas paints the theme background behind a full-width screen line.
func (t theme) canvas(line string, width int) string {
	if t.pal.Background == "" {
		return line
	}
	return t.paint(pad(line, width), t.pal.Background)
}

// swatch previews a palette as colored blocks.
func (t theme) swatch(p Palette) string {
	if !t.colored() {
		return ""
	}
	out := ""
	for _, value := range []string{p.Background, p.Accent, p.Success, p.Warn, p.Danger, p.Info} {
		if value == "" {
			out += "  "
			continue
		}
		out += lipgloss.NewStyle().Foreground(lipgloss.Color(value)).Render("██")
	}
	return out
}

// stageLook maps a node's execution state to its glyph and color. Glyphs are
// distinguishable without color; frame animates the running spinner.
func (t theme) stageLook(state string, frame int) (string, color.Color) {
	switch state {
	case "done":
		return "●", t.success()
	case "historical":
		return "◌", t.muted()
	case "running":
		spinner := []string{"◐", "◓", "◑", "◒"}
		return spinner[frame%len(spinner)], t.accent()
	case "verifying", "approved, publishing":
		return "◍", t.accent()
	case "approved, not published":
		return "✖", t.danger()
	case "awaiting-approval":
		return "◆", t.warn()
	case "recovering":
		return "⟳", t.warn()
	case "failed", "needs-attention":
		return "✖", t.danger()
	case "queued", "preparing":
		return "◔", t.muted()
	}
	return "○", t.muted()
}

// severity colors a detail line by what it reports, so failures and passing
// checks are findable without reading every line.
func (t theme) severity(s string) lipgloss.Style {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch {
	case strings.HasPrefix(lower, "error:"), strings.Contains(lower, "failed"), strings.Contains(lower, "must resolve"), strings.Contains(lower, "not probed"):
		return t.fg(t.danger())
	case strings.HasPrefix(lower, "supervisor:"), strings.Contains(lower, "approved"), strings.Contains(lower, "accepted"), strings.Contains(lower, "passed"):
		return t.fg(t.success())
	case strings.HasPrefix(lower, "live:"), strings.HasPrefix(lower, "worker attempt"), strings.HasPrefix(lower, "to worker"), strings.HasPrefix(lower, "to supervisor"):
		return t.fg(t.accent())
	case strings.HasPrefix(lower, "historical"), strings.HasPrefix(lower, "  "), strings.HasPrefix(lower, "["):
		return t.dim()
	}
	return t.style()
}

// pad renders s into exactly width cells, truncating with an ellipsis and
// padding with spaces. Width accounting ignores escape sequences.
func pad(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if ansi.StringWidth(s) > width {
		s = ansi.Truncate(s, width, "…")
	}
	if gap := width - ansi.StringWidth(s); gap > 0 {
		s += strings.Repeat(" ", gap)
	}
	return s
}

// box frames body in a rounded border exactly width cells wide, titling the top
// edge. It returns len(body)+2 lines; callers budget height accordingly.
func (t theme) box(title string, body []string, width int) []string {
	if width < 4 {
		return body
	}
	edge := t.fg(t.line())
	inner := width - 2
	label := ""
	if title != "" {
		label = "─ " + title + " "
		if ansi.StringWidth(label) > inner {
			label = ansi.Truncate(label, inner, "")
		}
	}
	top := "╭" + label + strings.Repeat("─", max(0, inner-ansi.StringWidth(label))) + "╮"
	if title != "" && t.colored() {
		plain := "─ "
		rest := strings.Repeat("─", max(0, inner-ansi.StringWidth(label)))
		top = edge.Render("╭"+plain) + t.bold().Render(strings.TrimSuffix(strings.TrimPrefix(label, "─ "), " ")) + edge.Render(" "+rest+"╮")
	} else {
		top = edge.Render(top)
	}
	lines := []string{top}
	for _, row := range body {
		lines = append(lines, edge.Render("│")+pad(row, inner)+edge.Render("│"))
	}
	return append(lines, edge.Render("╰"+strings.Repeat("─", inner)+"╯"))
}
