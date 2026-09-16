package tui

import (
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestConversationShowsEffectiveModelBeforeAnyAttempt(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 160, 50
	rev := m.Runs[0].Current()
	node := m.nodeID()
	nodeCfg := rev.Config.Workflow.Nodes[node]
	nodeCfg.Agents.Worker.Model = "fixture-worker-model"
	rev.Config.Workflow.Nodes[node] = nodeCfg
	rev.Config.Agents.Supervisor.Model = "fixture-supervisor-model"
	view := m.View().Content
	if !strings.Contains(view, "Worker model: fixture-worker-model · Supervisor model: fixture-supervisor-model") {
		t.Fatalf("conversation lacks the effective model before any attempt exists:\n%s", view)
	}
	if !strings.Contains(view, "No stage messages yet") {
		t.Fatalf("conversation lost the no-messages hint:\n%s", view)
	}
}

func TestConversationShowsHarnessDefaultWhenModelUnset(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 160, 50
	view := m.View().Content
	if !strings.Contains(view, "Worker model: harness default · Supervisor model: harness default") {
		t.Fatalf("conversation should show the harness default with nothing configured:\n%s", view)
	}
}

func TestConversationModelLineCoexistsWithAttemptDetail(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 160, 50
	rev := m.Runs[0].Current()
	node := m.nodeID()
	nodeCfg := rev.Config.Workflow.Nodes[node]
	nodeCfg.Agents.Worker.Model = "fixture-worker-model"
	rev.Config.Workflow.Nodes[node] = nodeCfg
	rev.Attempts = []workflow.Attempt{{ID: "attempt_one", Node: node, Number: 1, State: "running"}}
	view := m.View().Content
	if !strings.Contains(view, "Worker model: fixture-worker-model") || !strings.Contains(view, "Worker attempt 1: running") {
		t.Fatalf("conversation lost either the model line or the attempt detail:\n%s", view)
	}
}

func TestTheStageViewNamesTheModelThatActuallyRan(t *testing.T) {
	// The whole point: "harness default" never said which model that was.
	if got := stageModelTUI("", "claude-sonnet-5-20260115"); got != "claude-sonnet-5-20260115 (harness default)" {
		t.Fatalf("unconfigured role: %q", got)
	}
	// An alias shows the version behind it.
	if got := stageModelTUI("sonnet", "claude-sonnet-5-20260115"); got != "claude-sonnet-5-20260115 (configured sonnet)" {
		t.Fatalf("alias: %q", got)
	}
	// Agreement is not worth two names.
	if got := stageModelTUI("opus", "opus"); got != "opus" {
		t.Fatalf("agreement: %q", got)
	}
	// Nothing reported falls back rather than inventing.
	if got := stageModelTUI("", ""); got != "harness default" {
		t.Fatalf("nothing known: %q", got)
	}
	if got := stageModelTUI("opus", ""); got != "opus" {
		t.Fatalf("configured only: %q", got)
	}
}
