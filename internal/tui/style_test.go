package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/charmbracelet/x/ansi"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func styledFixture(t *testing.T, profile colorprofile.Profile, runs int) Model {
	t.Helper()
	m := modelFixture(t)
	base := m.Runs[0]
	for i := 1; i < runs; i++ {
		c := base.Current().Config
		r, err := workflow.NewRun("Run "+string(rune('A'+i)), "objective", "dev", c, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		m.Runs = append(m.Runs, *r)
	}
	next, _ := m.Update(tea.ColorProfileMsg{Profile: profile})
	return next.(Model)
}

func TestStyledDashboardFitsEveryTerminalSize(t *testing.T) {
	for _, profile := range []colorprofile.Profile{colorprofile.TrueColor, colorprofile.ANSI, colorprofile.Ascii} {
		m := styledFixture(t, profile, 6)
		for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 40}, {Width: 80, Height: 24}, {Width: 55, Height: 18}, {Width: 20, Height: 8}} {
			next, _ := m.Update(size)
			view := next.(Model).View().Content
			lines := strings.Split(view, "\n")
			if len(lines) > size.Height {
				t.Fatalf("profile %v %dx%d: %d lines", profile, size.Width, size.Height, len(lines))
			}
			for _, line := range lines {
				if w := ansi.StringWidth(line); w > size.Width {
					t.Fatalf("profile %v %dx%d: line width %d: %q", profile, size.Width, size.Height, w, ansi.Strip(line))
				}
			}
		}
	}
}

func TestSelectionReadsWithoutColor(t *testing.T) {
	m := styledFixture(t, colorprofile.Ascii, 3)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	m.Selected = 1
	plain := m.View().Content
	if strings.ContainsRune(plain, '\x1b') {
		t.Fatal("an uncolored profile emitted escape sequences")
	}
	if !strings.Contains(plain, "▸ "+m.Runs[1].Name) {
		t.Fatal("selected run has no visible marker")
	}
	if !strings.Contains(plain, "["+panels[m.Panel]+"]") {
		t.Fatal("active panel is not marked")
	}
	if !strings.Contains(plain, "[task ") {
		t.Fatal("selected stage is not marked")
	}
}

func TestColorProfileAddsStylingWithoutChangingText(t *testing.T) {
	colored := styledFixture(t, colorprofile.TrueColor, 2)
	next, _ := colored.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	colored = next.(Model)
	plain := colored
	plain.Theme.profile = colorprofile.Ascii
	styled := colored.View().Content
	if !strings.ContainsRune(styled, '\x1b') {
		t.Fatal("a color terminal rendered no styling")
	}
	normalize := func(view string) []string {
		lines := strings.Split(view, "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " ")
		}
		for len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	// A painted background pads every row to the screen; compare the text.
	if a, b := normalize(ansi.Strip(styled)), normalize(plain.View().Content); len(a) != len(b) {
		t.Fatalf("styling changed the line count: %d vs %d", len(a), len(b))
	} else {
		for i := range a {
			if a[i] != b[i] {
				t.Fatalf("styling changed line %d:\ncolored: %q\nplain:   %q", i, a[i], b[i])
			}
		}
	}
}

func TestClickSelectsTheRunDrawnOnThatRow(t *testing.T) {
	for _, height := range []int{40, 20} {
		m := styledFixture(t, colorprofile.TrueColor, 5)
		next, _ := m.Update(tea.WindowSizeMsg{Width: 100, Height: height})
		m = next.(Model)
		rows := strings.Split(ansi.Strip(m.View().Content), "\n")
		for y, row := range rows {
			for i, r := range m.Runs {
				if !strings.Contains(row, r.Name) || strings.Contains(row, "·") {
					continue
				}
				clicked, _ := m.Update(tea.MouseClickMsg{X: 4, Y: y, Button: tea.MouseLeft})
				if got := clicked.(Model).Selected; got != i {
					t.Fatalf("height %d: clicked row %d showing %q selected run %d", height, y, r.Name, got)
				}
			}
		}
	}
}
