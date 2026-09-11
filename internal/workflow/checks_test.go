package workflow

import "testing"

func TestChecksChooseOneExecutorAndCanDeclareMissingPluginBeforePlan(t *testing.T) {
	c := fixture(t)
	qa := c.Workflow.Nodes["qa"]
	qa.Checks = []Check{{Name: "browser", Plugin: "playwright", Input: map[string]any{"path": "/"}}}
	c.Workflow.Nodes["qa"] = qa
	if err := c.Validate(); err != nil {
		t.Fatal("candidate cannot declare its missing check capability", err)
	}
	qa.Checks[0].Command = []string{"echo", "fake"}
	c.Workflow.Nodes["qa"] = qa
	if err := c.Validate(); err == nil {
		t.Fatal("ambiguous check executor accepted")
	}
	qa.Checks[0].Plugin = ""
	c.Workflow.Nodes["qa"] = qa
	if err := c.Validate(); err == nil {
		t.Fatal("command check accepted plugin input")
	}
}
