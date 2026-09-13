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
}

func (t theme) colored() bool {
	switch t.profile {
	case colorprofile.ANSI, colorprofile.ANSI256, colorprofile.TrueColor:
		return true
	}
	return false
}

// pick chooses between a light-background and a dark-background value. Bubble
// Tea reports the background once at startup and whenever it changes.
func (t theme) pick(light, dark string) color.Color {
	if t.dark {
		return lipgloss.Color(dark)
	}
	return lipgloss.Color(light)
}

func (t theme) accent() color.Color  { return t.pick("#0b5fa5", "#7aa2f7") }
func (t theme) success() color.Color { return t.pick("#1a7f37", "#7ee787") }
func (t theme) danger() color.Color  { return t.pick("#b3261e", "#ff7b72") }
func (t theme) warn() color.Color    { return t.pick("#9a6700", "#e3b341") }
func (t theme) muted() color.Color   { return t.pick("#6e7781", "#8b949e") }
func (t theme) line() color.Color    { return t.pick("#d0d7de", "#3d444d") }

// style returns a no-op style unless the terminal supports color, so bold and
// faint attributes never reach a terminal that reported none.
func (t theme) style() lipgloss.Style { return lipgloss.NewStyle() }

func (t theme) fg(c color.Color) lipgloss.Style {
	if !t.colored() {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(c)
}
func (t theme) strong(c color.Color) lipgloss.Style {
	if !t.colored() {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(c).Bold(true)
}
func (t theme) bold() lipgloss.Style {
	if !t.colored() {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Bold(true)
}
func (t theme) dim() lipgloss.Style { return t.fg(t.muted()) }

// selected marks the focused row without relying on color alone: the caret in
// the row text carries the same meaning on a monochrome terminal.
func (t theme) selected() lipgloss.Style {
	if !t.colored() {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(t.accent()).Bold(true)
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
	case "verifying":
		return "◍", t.accent()
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
