package tui

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type createAPI struct {
	fakeAPI
	created []daemon.CreateRequest
	err     error
}

func (c *createAPI) Create(_ context.Context, req daemon.CreateRequest) (*workflow.Run, error) {
	c.created = append(c.created, req)
	if c.err != nil {
		return nil, c.err
	}
	return workflow.NewRun(req.Name, req.Task, req.Owner, req.Config, time.Now())
}

// drive runs a command and feeds its message back, the way the Bubble Tea
// runtime does, following any refresh the update schedules.
func drive(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	for i := 0; cmd != nil && i < 4; i++ {
		msg := cmd()
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				m = drive(t, m, c)
			}
			return m
		}
		next, following := m.Update(msg)
		m, cmd = next.(Model), following
	}
	return m
}

func typeText(m Model, text string) Model {
	for _, r := range text {
		next, _ := m.Update(tea.KeyPressMsg{Code: r, Text: string(r)})
		m = next.(Model)
	}
	return m
}

func TestNewRunErrorStaysVisibleAfterRefresh(t *testing.T) {
	api := &createAPI{}
	m := New(api, t.TempDir()) // no envctl.yaml here
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)

	if hint := ansi.Strip(m.View().Content); !strings.Contains(hint, "no envctl.yaml there yet") {
		t.Fatal("empty dashboard does not explain the missing configuration")
	}
	m, _ = key(m, "n")
	if m.Mode != "new" {
		t.Fatal("n did not open the new-run input")
	}
	m = typeText(m, "Add CSV export")
	m, cmd := key(m, "enter")
	m = drive(t, m, cmd)

	if len(api.created) != 0 {
		t.Fatal("a run was requested without a workflow configuration")
	}
	view := ansi.Strip(m.View().Content)
	if m.Error == "" || !strings.Contains(view, "envctl.yaml") {
		t.Fatalf("the failure was not visible after the refresh: error=%q", m.Error)
	}
	// The next successful refresh must not silently clear it either.
	m = drive(t, m, m.refresh())
	if m.Error == "" {
		t.Fatal("a routine refresh cleared the create error")
	}
	// Starting another action clears the stale error.
	m, _ = key(m, "n")
	if m.Error != "" {
		t.Fatal("opening a new input kept a stale error")
	}
}

func TestNewRunCreatesFromTheRepositoryConfiguration(t *testing.T) {
	root := t.TempDir()
	config := "version: 2\nproject: shop\nrepositories: [{id: app, url: https://github.com/you/shop.git, ref: main}]\nworkflow: {template: feature}\n"
	if err := os.WriteFile(filepath.Join(root, "envctl.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &createAPI{}
	m := New(api, root)
	m, _ = key(m, "n")
	m = typeText(m, "Add CSV export")
	m, cmd := key(m, "enter")
	m = drive(t, m, cmd)
	if len(api.created) != 1 || api.created[0].Task != "Add CSV export" || m.Error != "" {
		t.Fatalf("run not created: %d requests, error %q", len(api.created), m.Error)
	}

	api.err = errors.New("coordinator refused the run")
	m, _ = key(m, "n")
	m = typeText(m, "Second")
	m, cmd = key(m, "enter")
	m = drive(t, m, cmd)
	if !strings.Contains(m.Error, "coordinator refused the run") {
		t.Fatalf("API failure hidden: %q", m.Error)
	}
}

func TestNewRunSelectsANamedWorkflowWithTab(t *testing.T) {
	root := t.TempDir()
	config := "version: 2\nproject: shop\nrepositories: [{id: app, url: https://github.com/you/shop.git, ref: main}]\nworkflow: {template: feature}\nworkflows: {small: {template: small, nodes: {build: {checks: [{name: unit, command: [go, test, ./...]}]}}}}\n"
	if err := os.WriteFile(filepath.Join(root, "envctl.yaml"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	api := &createAPI{}
	m := New(api, root)
	next, _ := m.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	m = next.(Model)
	m, _ = key(m, "n")
	if m.NewWorkflow != "default" || !strings.Contains(ansi.Strip(m.View().Content), "default workflow (tab to change)") {
		t.Fatalf("new-run input does not offer the workflows: %q", m.NewWorkflow)
	}
	m, _ = key(m, "tab")
	if m.NewWorkflow != "small" || !strings.Contains(ansi.Strip(m.View().Content), "small workflow") {
		t.Fatalf("tab did not select the small workflow: %q", m.NewWorkflow)
	}
	m = typeText(m, "Add a flag")
	m, cmd := key(m, "enter")
	m = drive(t, m, cmd)
	if len(api.created) != 1 || m.Error != "" {
		t.Fatalf("run not created: %d requests, error %q", len(api.created), m.Error)
	}
	if c := api.created[0].Config; c.WorkflowName != "small" || len(c.Workflow.Nodes) != 3 || c.Workflows != nil {
		t.Fatalf("created with the wrong workflow: %q, %d stages", c.WorkflowName, len(c.Workflow.Nodes))
	}
	run, err := workflow.NewRun("Add a flag", "Add a flag", "local", api.created[0].Config, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	m.Runs = []workflow.Run{*run}
	if !strings.Contains(ansi.Strip(m.View().Content), "small workflow") {
		t.Fatal("the run summary does not name its workflow")
	}
}
