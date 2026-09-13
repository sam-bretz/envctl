package tui

import (
	"fmt"

	"github.com/charmbracelet/x/ansi"
)

// themePicker previews each theme as the selection moves, like a colorscheme
// picker. Enter keeps and saves it; escape restores the theme it replaced.
type themePicker struct {
	names   []string
	index   int
	restore string // preview in effect before the picker opened
}

func (m *Model) openPicker() {
	names := ThemeNames()
	current := m.Theme.config.Name
	if m.Theme.override != "" {
		current = m.Theme.override
	}
	if current == "" {
		current = Auto
	}
	index := 0
	for i, name := range names {
		if name == current {
			index = i
		}
	}
	m.Picker = &themePicker{names: names, index: index, restore: m.Theme.preview}
	m.Offset = 0
}

func (m Model) updatePicker(key string) Model {
	p := *m.Picker
	switch key {
	case "up", "k":
		p.index = (p.index + len(p.names) - 1) % len(p.names)
	case "down", "j", "tab":
		p.index = (p.index + 1) % len(p.names)
	case "esc", "q", "T":
		m.Theme.preview = p.restore
		m.Theme.resolve()
		m.Picker = nil
		return m
	case "enter":
		name := p.names[p.index]
		m.Picker = nil
		m.Theme.preview = ""
		m.Theme.config.Name = name
		m.Theme.override = ""
		m.Theme.resolve()
		if m.ConfigPath == "" {
			m.Notice = "Theme: " + m.Theme.name() + " (not saved)"
			return m
		}
		if err := SaveThemeName(m.ConfigPath, name); err != nil {
			m.Error = "Theme applied but not saved: " + err.Error()
			return m
		}
		m.Error = ""
		m.Notice = "Theme saved: " + name + " (" + m.ConfigPath + ")"
		return m
	default:
		return m
	}
	m.Picker = &p
	m.Theme.preview = p.names[p.index]
	if m.Theme.preview == Auto {
		// Preview auto as what it would resolve to on this terminal.
		m.Theme.preview = ""
		saved := m.Theme.config.Name
		m.Theme.config.Name = Auto
		m.Theme.resolve()
		m.Theme.config.Name = saved
		return m
	}
	m.Theme.resolve()
	return m
}

// pickerLines renders the theme list for the detail frame. Theme names are
// built-in constants, so these lines skip content sanitization.
func (m Model) pickerLines(width, room int) []string {
	t := m.Theme
	p := m.Picker
	lines := []string{t.dim().Render("↑/↓ preview · enter keep and save · esc cancel"), ""}
	start := max(0, min(p.index-(room-3)/2, len(p.names)-(room-2)))
	for i := start; i < len(p.names) && len(lines) < room; i++ {
		name := p.names[i]
		detail := ""
		if name == Auto {
			auto := t.config
			auto.Name = Auto
			detail = "follows your terminal: " + auto.Resolve(false).Name + " / " + auto.Resolve(true).Name
		} else if pal, ok := Lookup(name); ok {
			mode := "light"
			if pal.Dark {
				mode = "dark"
			}
			if name == "terminal" {
				mode = "your terminal's colors"
			}
			detail = t.swatch(pal) + " " + t.dim().Render(mode)
		}
		label := fmt.Sprintf("%-18s", name)
		row := "  " + t.style().Render(label) + " " + detail
		if i == p.index {
			row = t.highlight(t.strong(t.accent()).Render("▸ "+label) + " " + detail)
		}
		lines = append(lines, ansi.Truncate(row, max(0, width-2), "…"))
	}
	if saved := t.config.Name; saved != "" && room > len(lines) {
		lines = append(lines, "", t.dim().Render("saved: "+saved))
	}
	return lines
}
