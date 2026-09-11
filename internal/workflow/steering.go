package workflow

import (
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Targets reports whether a user message should reach the given agent role of
// an attempt at node. Supervisors review every message for their stage so they
// can reject work that ignores steering addressed to the worker.
func (m Message) Targets(node, role string) bool {
	if m.Node != "" && m.Node != node {
		return false
	}
	return role == "supervisor" || m.Recipient == role
}

// BoundActivity keeps the newest lines, each a single printable line within
// the display bound. Activity is advisory and never evidence.
func BoundActivity(lines []string) []string {
	var out []string
	for _, line := range lines {
		line = strings.Map(func(r rune) rune {
			if r == '\n' || r == '\r' || r == '\t' {
				return ' '
			}
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if len(line) > ProgressLineBytes {
			cut := ProgressLineBytes - len("…")
			for cut > 0 && !utf8.RuneStart(line[cut]) {
				cut--
			}
			line = line[:cut] + "…"
		}
		out = append(out, line)
	}
	if len(out) > ProgressActivityLines {
		out = out[len(out)-ProgressActivityLines:]
	}
	return out
}

// SameProgress compares display content, ignoring when it was observed.
func SameProgress(a, b *Progress) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Phase == b.Phase && a.Detail == b.Detail && a.Generation == b.Generation && slices.Equal(a.Activity, b.Activity)
}

// MessageStatus explains where a user message has been delivered, so steering
// is never silently dropped from the reviewer's view.
func (v *Revision) MessageStatus(m Message) string {
	var parts []string
	for _, a := range v.Attempts {
		for _, d := range a.Steering {
			if d.Message != m.ID {
				continue
			}
			if d.Generation == 0 {
				parts = append(parts, fmt.Sprintf("included when %s %s attempt %d started", a.Node, d.Role, a.Number))
			} else {
				parts = append(parts, fmt.Sprintf("delivered live to %s %s attempt %d (resume %d)", a.Node, d.Role, a.Number, d.Generation))
			}
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "; ")
	}
	for _, a := range v.Attempts {
		if a.State == "running" && m.Targets(a.Node, m.Recipient) {
			return fmt.Sprintf("pending live delivery to %s attempt %d", a.Node, a.Number)
		}
	}
	if m.Node != "" {
		return "queued for the next " + m.Node + " attempt"
	}
	return "queued for the next attempt"
}
