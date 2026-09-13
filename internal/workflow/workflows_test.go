package workflow

import (
	"strings"
	"testing"
	"time"
)

const workflowsBase = "version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\n"

const buildChecks = "checks: [{name: unit, command: [go, test, ./...]}]"

func TestSmallTemplateNeedsChecksBeforeTheApprovedChange(t *testing.T) {
	_, err := Parse([]byte(workflowsBase + "workflow: {template: small}\n"))
	if err == nil || !strings.Contains(err.Error(), "approved change") {
		t.Fatalf("a small workflow without checks was accepted: %v", err)
	}
	c, err := Parse([]byte(workflowsBase + "workflow: {template: small, nodes: {build: {" + buildChecks + "}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	w := c.Workflow
	if len(w.Nodes) != 3 || w.PlanID() != "plan" || len(w.Nodes["plan"].Needs) != 0 {
		t.Fatalf("small is not Plan → Build → Approve: %+v", w.Nodes)
	}
	if !w.Descendants("plan")["build"] || !w.Descendants("build")["approved-change"] || w.Nodes["approved-change"].Gate != "human" {
		t.Fatalf("small stages out of order: %+v", w.Nodes)
	}
	if !w.Nodes["build"].verifies() {
		t.Fatal("build with checks does not verify the change")
	}
}

func TestTaskIsOptionalButPlanMustBeTheRootWithoutIt(t *testing.T) {
	custom := "workflow: {nodes: {plan: {kind: plan, needs: [prep]}, prep: {kind: design}, build: {kind: code, needs: [plan], writes: ['*'], " + buildChecks + "}, ship: {kind: change, needs: [build], gate: human}}}\n"
	if _, err := Parse([]byte(workflowsBase + custom)); err == nil {
		t.Fatal("a workflow without Task accepted a stage before Plan")
	}
	if _, err := Parse([]byte(workflowsBase + "workflow: {template: feature}\n")); err != nil {
		t.Fatal("the feature template no longer validates:", err)
	}
	if _, err := Parse([]byte(workflowsBase + "workflow: {template: tiny}\n")); err == nil || !strings.Contains(err.Error(), "small") {
		t.Fatalf("an unknown template did not list the built-ins: %v", err)
	}
}

func TestNamedWorkflowsValidateAndSelect(t *testing.T) {
	raw := workflowsBase + "workflow: {template: feature}\nworkflows:\n  small: {template: small, nodes: {build: {" + buildChecks + "}}}\n  docs: {template: small, nodes: {build: {prompt: docs only, " + buildChecks + "}}}\n"
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(c.WorkflowNames(), ","); got != "default,docs,small" {
		t.Fatalf("workflow names %s", got)
	}
	small, err := c.SelectWorkflow("small")
	if err != nil {
		t.Fatal(err)
	}
	if small.WorkflowName != "small" || small.Workflows != nil || len(small.Workflow.Nodes) != 3 {
		t.Fatalf("selected configuration: name %q workflows %v nodes %d", small.WorkflowName, small.Workflows, len(small.Workflow.Nodes))
	}
	if c.Workflows == nil || len(c.Workflow.Nodes) != 6 {
		t.Fatal("selection changed the loaded configuration")
	}
	def, err := c.SelectWorkflow("")
	if err != nil || def.WorkflowName != "" || def.Workflows != nil || len(def.Workflow.Nodes) != 6 {
		t.Fatalf("default selection: %+v %v", def.WorkflowName, err)
	}
	if _, err := c.SelectWorkflow("huge"); err == nil || !strings.Contains(err.Error(), "default, docs, small") {
		t.Fatalf("unknown workflow error did not list choices: %v", err)
	}
	if _, err := NewRun("small", "add a flag", "tester", small, time.Now()); err != nil {
		t.Fatal("a run cannot start from the small workflow:", err)
	}

	for bad, want := range map[string]string{
		"workflows: {small: {template: small}}\n":                                          "workflows.small:",
		"workflows: {default: {template: small, nodes: {build: {" + buildChecks + "}}}}\n": "default",
		"workflows: {Big One: {template: feature}}\n":                                      "Big One",
	} {
		if _, err := Parse([]byte(workflowsBase + "workflow: {template: feature}\n" + bad)); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("invalid named workflow %q: %v", bad, err)
		}
	}
}

func TestNamedWorkflowsKeepTheDefaultConfigurationDigest(t *testing.T) {
	plain, err := Parse([]byte(workflowsBase + "workflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	named, err := Parse([]byte(workflowsBase + "workflow: {template: feature}\nworkflows: {small: {template: small, nodes: {build: {" + buildChecks + "}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	selected, err := named.SelectWorkflow(DefaultWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	if Digest(selected) != Digest(plain) {
		t.Fatal("adding named workflows changed the default workflow's identity")
	}
	small, err := named.SelectWorkflow("small")
	if err != nil {
		t.Fatal(err)
	}
	if Digest(small) == Digest(plain) {
		t.Fatal("the small workflow shares the default's identity")
	}
}
