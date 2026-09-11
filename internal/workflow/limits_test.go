package workflow

import (
	"encoding/json"
	"strings"
	"testing"
)

const limitsBase = "version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nlimits: {max_attempts: 5, attempt_seconds: 600}\n"

func TestNodeLimitsOverrideRevisionBudgetAndValidate(t *testing.T) {
	c, err := Parse([]byte(limitsBase + "workflow: {template: feature, nodes: {qa: {limits: {max_attempts: 2, attempt_seconds: 7200}}, code: {limits: {attempt_seconds: 900}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.NodeLimits("qa"); got.MaxAttempts != 2 || got.AttemptSeconds != 7200 || got.Parallel != c.Limits.Parallel || got.VMs != c.Limits.VMs {
		t.Fatalf("qa override not applied: %+v", got)
	}
	if got := c.NodeLimits("code"); got.MaxAttempts != 5 || got.AttemptSeconds != 900 {
		t.Fatalf("partial override did not inherit the revision budget: %+v", got)
	}
	if got := c.NodeLimits("design"); got != c.Limits {
		t.Fatalf("node without override differs from revision limits: %+v", got)
	}
	for _, bad := range []string{"{max_attempts: -1}", "{attempt_seconds: -5}", "{max_attempts: 101}", "{attempt_seconds: 86401}", "{retries: 3}"} {
		if _, err := Parse([]byte(limitsBase + "workflow: {template: feature, nodes: {qa: {limits: " + bad + "}}}\n")); err == nil {
			t.Fatalf("invalid node limits %s accepted", bad)
		}
	}
}

func TestNodeLimitsOmittedKeepConfigurationDigest(t *testing.T) {
	plain, err := Parse([]byte(limitsBase + "workflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := Parse([]byte(limitsBase + "workflow: {template: feature, nodes: {qa: {limits: {}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Digest(plain) != Digest(empty) {
		t.Fatal("an empty override changed the configuration identity")
	}
	raw, err := json.Marshal(plain.Workflow.Nodes["qa"])
	if err != nil || strings.Contains(string(raw), "limits") {
		t.Fatalf("absent override serialized into node identity: %s", raw)
	}
	override, err := Parse([]byte(limitsBase + "workflow: {template: feature, nodes: {qa: {limits: {max_attempts: 2}}}}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if Digest(override) == Digest(plain) || Digest(override.Workflow.Nodes["qa"]) == Digest(plain.Workflow.Nodes["qa"]) {
		t.Fatal("a real override did not change the node identity")
	}
}
