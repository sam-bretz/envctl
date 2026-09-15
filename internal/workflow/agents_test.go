package workflow

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

const agentsBase = "version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nagents: {worker: {model: gpt-5-worker}, supervisor: {model: gpt-5-supervisor}}\n"

func TestNodeAgentsOverrideModelPerRoleAndValidate(t *testing.T) {
	c, err := Parse([]byte(agentsBase + "workflow: {template: feature, nodes: {qa: {agents: {worker: {model: qa-worker-model}}}, code: {agents: {supervisor: {model: code-supervisor-model}}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NodeAgents("qa"); got.Worker.Model != "qa-worker-model" || got.Supervisor.Model != "gpt-5-supervisor" {
		t.Fatalf("qa worker override not applied independently of supervisor: %+v", got)
	}
	if got := c.NodeAgents("code"); got.Worker.Model != "gpt-5-worker" || got.Supervisor.Model != "code-supervisor-model" {
		t.Fatalf("code supervisor override not applied independently of worker: %+v", got)
	}
	if got := c.NodeAgents("design"); got.Worker.Model != c.Agents.Worker.Model || got.Supervisor.Model != c.Agents.Supervisor.Model {
		t.Fatalf("node without override differs from run-wide agents: %+v", got)
	}
	for _, bad := range []string{"{worker: {kind: codex}}", "{worker: {version: 1}}", "{reviewer: {model: x}}"} {
		if _, err := Parse([]byte(agentsBase + "workflow: {template: feature, nodes: {qa: {agents: " + bad + "}}}\n")); err == nil {
			t.Fatalf("invalid node agents override %s accepted", bad)
		}
	}
}

func TestNodeAgentsUnsetInheritsHarnessDefault(t *testing.T) {
	c, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NodeAgents("code"); got.Worker.Model != "" || got.Supervisor.Model != "" {
		t.Fatalf("expected harness default (empty model) with nothing configured: %+v", got)
	}
}

func TestNodeAgentsOmittedKeepConfigurationDigest(t *testing.T) {
	plain, err := Parse([]byte(agentsBase + "workflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := Parse([]byte(agentsBase + "workflow: {template: feature, nodes: {qa: {agents: {}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Digest(plain) != Digest(empty) {
		t.Fatal("an empty override changed the configuration identity")
	}
	raw, err := json.Marshal(plain.Workflow.Nodes["qa"])
	if err != nil || strings.Contains(string(raw), "agents") {
		t.Fatalf("absent override serialized into node identity: %s", raw)
	}
	override, err := Parse([]byte(agentsBase + "workflow: {template: feature, nodes: {qa: {agents: {worker: {model: qa-model}}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Digest(override) == Digest(plain) || Digest(override.Workflow.Nodes["qa"]) == Digest(plain.Workflow.Nodes["qa"]) {
		t.Fatal("a real override did not change the node identity")
	}
	if d := Digest(override.Workflow.Nodes["code"]); d != Digest(plain.Workflow.Nodes["code"]) {
		t.Fatal("an override on one node perturbed a different node's identity")
	}
}

func TestBeginRecordsResolvedModelsPerNode(t *testing.T) {
	c, err := Parse([]byte(agentsBase + "workflow: {template: feature, nodes: {task: {agents: {worker: {model: task-worker-model}}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRun("test", "exercise model attribution", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rev := r.Current()
	rev.State = "active"
	rev.Runtime = RuntimeState{ID: "vm-one", Ready: true}
	a, err := rev.Begin("task", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Models) != 2 || a.Models["worker"] != "task-worker-model" || a.Models["supervisor"] != "gpt-5-supervisor" {
		t.Fatalf("task attempt did not record its resolved models: %+v", a.Models)
	}
}

func TestBeginRecordsNoModelsWhenNoneConfigured(t *testing.T) {
	c, err := Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := NewRun("test", "exercise model attribution", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rev := r.Current()
	rev.State = "active"
	rev.Runtime = RuntimeState{ID: "vm-one", Ready: true}
	a, err := rev.Begin("task", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a.Models != nil {
		t.Fatalf("expected nil Models with nothing configured: %+v", a.Models)
	}
}
