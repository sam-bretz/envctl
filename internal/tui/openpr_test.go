package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestOpenPullRequestKey(t *testing.T) {
	m := modelFixture(t)
	var opened []string
	m.OpenURL = func(url string) error { opened = append(opened, url); return nil }

	m, cmd := key(m, "g")
	if cmd != nil || !strings.Contains(m.Error, "no pull request yet") {
		t.Fatalf("no feedback before publication: %q", m.Error)
	}

	v := m.Runs[0].Current()
	v.Checkpoints["approved-change"] = workflow.Checkpoint{Node: "approved-change", Result: workflow.Result{PRs: map[string]string{
		"app":        "https://github.com/you/app/pull/7",
		"app/vendor": "https://github.com/you/vendor/pull/3",
		"bad":        "javascript:alert(1)",
	}}}
	m.Node = 0 // any stage opens the run's pull requests
	m, cmd = key(m, "g")
	if cmd == nil {
		t.Fatal("g did not open the pull requests")
	}
	next, _ := m.Update(cmd())
	m = next.(Model)
	if len(opened) != 2 || opened[0] != "https://github.com/you/app/pull/7" || opened[1] != "https://github.com/you/vendor/pull/3" {
		t.Fatalf("opened %v", opened)
	}
	if !strings.Contains(m.Notice, "Opened 2 pull requests") {
		t.Fatalf("notice %q", m.Notice)
	}

	m.OpenURL = func(string) error { return errors.New("no opener") }
	m, cmd = key(m, "g")
	next, _ = m.Update(cmd())
	if got := next.(Model).Error; !strings.Contains(got, "could not open the pull request: no opener") {
		t.Fatalf("opener failure hidden: %q", got)
	}
}
