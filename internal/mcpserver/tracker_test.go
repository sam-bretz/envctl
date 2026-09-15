package mcpserver

import (
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestMCPCreateForwardsTaskRef(t *testing.T) {
	api, _ := fixture(t)
	s := session(t, api, Options{Version: "test"})
	result := call(t, s, "envctl_create", createInput{
		OperationID: "tracked", Name: "MCP feature", Task: "Build a feature", TaskRef: "ENG-123", Owner: "test",
		Directory:  t.TempDir(),
		ConfigYAML: "version: 2\nproject: mcp\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n",
	})
	var r workflow.Run
	decode(t, result, "run", &r)
	if r.TaskRef != "ENG-123" {
		t.Fatalf("task_ref not forwarded: %q", r.TaskRef)
	}
}
