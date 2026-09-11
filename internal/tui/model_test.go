package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type fakeAPI struct {
	request daemon.ActionRequest
	diff    review.Request
}

func (f *fakeAPI) List(context.Context) ([]workflow.Run, error) { return nil, nil }
func (f *fakeAPI) Create(context.Context, daemon.CreateRequest) (*workflow.Run, error) {
	return nil, errors.New("not used")
}
func (f *fakeAPI) Action(_ context.Context, _ string, r daemon.ActionRequest) (*workflow.Run, error) {
	f.request = r
	return nil, nil
}
func (f *fakeAPI) Artifact(context.Context, string) ([]byte, error) { return []byte("artifact"), nil }
func (f *fakeAPI) Diff(_ context.Context, id string, req review.Request) (review.Comparison, error) {
	f.diff = req
	return review.Comparison{Run: id, Repositories: []review.RepositoryDiff{{Repository: "app", Before: "old", After: "new", Patch: "-old\n+new"}}}, nil
}
func modelFixture(t *testing.T) Model {
	t.Helper()
	c, e := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if e != nil {
		t.Fatal(e)
	}
	r, e := workflow.NewRun("Exports", "Add account-level exports", "dev", c, time.Now())
	if e != nil {
		t.Fatal(e)
	}
	m := New(&fakeAPI{}, ".")
	m.Runs = []workflow.Run{*r}
	return m
}
func key(m Model, key string) (Model, tea.Cmd) {
	code := rune(0)
	if key == "enter" {
		code = tea.KeyEnter
	} else if key == "esc" {
		code = tea.KeyEscape
	} else if key == "tab" {
		code = tea.KeyTab
	} else if len(key) == 1 {
		code = rune(key[0])
	}
	next, cmd := m.Update(tea.KeyPressMsg{Code: code, Text: func() string {
		if len(key) == 1 {
			return key
		}
		return ""
	}()})
	return next.(Model), cmd
}
func TestDashboardResizeAndReadiness(t *testing.T) {
	m := modelFixture(t)
	for _, size := range []tea.WindowSizeMsg{{Width: 120, Height: 40}, {Width: 55, Height: 18}, {Width: 20, Height: 8}} {
		next, _ := m.Update(size)
		m = next.(Model)
		view := m.View()
		if !view.AltScreen {
			t.Fatal("not full screen")
		}
		lines := strings.Split(view.Content, "\n")
		if len(lines) > size.Height {
			t.Fatal("height overflow")
		}
		for _, line := range lines {
			if ansi.StringWidth(line) > size.Width {
				t.Fatal("width overflow", line)
			}
		}
	}
	m.Width = 120
	m.Height = 40
	m.Panel = 5
	if !strings.Contains(m.View().Content, "harness.worker: not probed") {
		t.Fatal("missing readiness not visible")
	}
}
func TestChatUsesSnapshotVersionAndRecipient(t *testing.T) {
	m := modelFixture(t)
	m, _ = key(m, "s")
	m, _ = key(m, "i")
	m.Input = "Check authorization"
	m, cmd := key(m, "enter")
	if cmd == nil {
		t.Fatal("no message command")
	}
	cmd()
	api := m.API.(*fakeAPI)
	if api.request.Recipient != "worker" || api.request.ExpectedVersion != 1 || api.request.Revision != m.Runs[0].CurrentRevision {
		t.Fatal(api.request)
	}
}
func TestNavigationNeverCancelsExecution(t *testing.T) {
	m := modelFixture(t)
	_, cmd := key(m, "q")
	if cmd == nil {
		t.Fatal("no detach")
	}
	if _, ok := cmd().(tea.QuitMsg); !ok {
		t.Fatal("not detach")
	}
	if m.API.(*fakeAPI).request.Action != "" {
		t.Fatal("detach changed execution")
	}
}
func TestUntrustedContentCannotInjectTerminalControls(t *testing.T) {
	m := modelFixture(t)
	m.Runs[0].Name = "hello\x1b[2J\x1b]52;c;c2VjcmV0\a"
	if strings.Contains(m.View().Content, "\x1b") {
		t.Fatal("terminal control escaped sanitization")
	}
}
func TestRefreshPreservesSelectedRun(t *testing.T) {
	m := modelFixture(t)
	a := m.Runs[0]
	b := workflow.Clone(a)
	b.ID = "other"
	m.Runs = []workflow.Run{a, b}
	m.Selected = 1
	next, _ := m.Update(snapshotMsg{runs: []workflow.Run{b, a}})
	if next.(Model).Selected != 0 {
		t.Fatal("selection jumped after reorder")
	}
}
