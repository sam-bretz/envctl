package tui

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// TestRenderDemoScreens writes ANSI captures for visual review. Opt-in only.
func TestRenderDemoScreens(t *testing.T) {
	out := os.Getenv("ENVCTL_RENDER_DEMO")
	if out == "" {
		t.Skip("set ENVCTL_RENDER_DEMO to an output directory")
	}
	c, err := workflow.Parse([]byte("version: 2\nproject: shop\nrepositories: [{id: app, url: https://github.com/you/shop.git, ref: main}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	mk := func(name, objective string) workflow.Run {
		r, err := workflow.NewRun(name, objective, "sam", c, now)
		if err != nil {
			t.Fatal(err)
		}
		return *r
	}
	checkpoint := func(v *workflow.Revision, node, summary string) {
		v.Checkpoints[node] = workflow.Checkpoint{ID: "cp_" + node, Node: node, Result: workflow.Result{Summary: summary, Review: workflow.Review{Accepted: true, Summary: "Matches the plan; tests cover the export path."}}}
	}
	active := mk("orders csv export", "Add CSV export to the orders report, streaming large result sets")
	v := active.Current()
	v.State = "active"
	checkpoint(v, "task", "Export orders as CSV from the report page")
	checkpoint(v, "plan", "Stream rows through the existing writer; add a download endpoint")
	checkpoint(v, "design", "Download button in the report toolbar; progress for >10k rows")
	v.Attempts = append(v.Attempts, workflow.Attempt{ID: "attempt_code", Node: "code", State: "running", Number: 1,
		Progress: &workflow.Progress{Phase: "worker", UpdatedAt: now, Activity: []string{
			"agent: Reading lib/export.ts to reuse the CSV writer",
			"ran (exit 0): npm test -- export",
			"agent: Adding GET /orders/export.csv with streaming",
		}}})
	v.Messages = append(v.Messages, workflow.Message{ID: "msg_1", Node: "code", Recipient: "worker", Body: "Use the existing CSV writer in lib/export.ts", CreatedAt: now})
	v.Attempts[0].Steering = []workflow.Delivery{{Message: "msg_1", Role: "worker", Generation: 1, At: now}}

	review := mk("tax rate api", "Expose tax rates per region through the public API")
	rv := review.Current()
	rv.State = "active"
	for _, n := range []string{"task", "plan", "design", "code", "qa"} {
		checkpoint(rv, n, "done")
	}
	rv.Attempts = append(rv.Attempts, workflow.Attempt{ID: "attempt_change", Node: "approved-change", State: "awaiting-approval", Number: 1})

	failed := mk("login rate limit", "Rate-limit failed logins per account")
	fv := failed.Current()
	fv.State = "needs-attention"
	checkpoint(fv, "task", "done")
	fv.Attempts = append(fv.Attempts, workflow.Attempt{ID: "attempt_plan", Node: "plan", State: "failed", Number: 3, Error: "publication.pr: gh is not authenticated"})

	for _, name := range []string{"catppuccin", "tokyo-night", "gruvbox-light", "rose-pine-dawn", "terminal"} {
		m := New(&fakeAPI{}, "/Users/sam/code/shop")
		m.Runs = []workflow.Run{active, review, failed}
		m.Node = 3
		next, _ := m.Update(tea.ColorProfileMsg{Profile: colorprofile.TrueColor})
		m = next.(Model)
		m.Theme.config = ThemeConfig{Name: name}
		next, _ = m.Update(tea.BackgroundColorMsg{})
		m = next.(Model)
		next, _ = m.Update(tea.WindowSizeMsg{Width: 118, Height: 34})
		m = next.(Model)
		if err := os.WriteFile(filepath.Join(out, name+".ans"), []byte(m.View().Content), 0o600); err != nil {
			t.Fatal(err)
		}
		if name == "catppuccin" {
			m.openPicker()
			m = m.updatePicker("j")
			m = m.updatePicker("j")
			if err := os.WriteFile(filepath.Join(out, "picker.ans"), []byte(m.View().Content), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
}
