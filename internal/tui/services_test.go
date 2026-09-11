package tui

import (
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestServicesPanelSeparatesHostPreviewFromGuestEndpoints(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 120, 40
	rev := m.Runs[0].Current()
	rev.Runtime = workflow.RuntimeState{ID: "envctl-rev-a", State: "running", Ready: true, PreviewURL: "http://127.0.0.1:41234/", Services: []workflow.Service{{Name: "web", State: "running/healthy", URL: "http://127.0.0.1:18089"}, {Name: "db", State: "running"}}}
	m.Panel = 4
	view := m.View().Content
	for _, want := range []string{"Preview: http://127.0.0.1:41234/", "Preview (this machine): http://127.0.0.1:41234/", "Services (endpoints are inside the VM):", "http://127.0.0.1:18089"} {
		if !strings.Contains(view, want) {
			t.Fatalf("services panel missing %q:\n%s", want, view)
		}
	}
	rev.Runtime.PreviewURL = ""
	if strings.Contains(m.View().Content, "Preview (this machine)") {
		t.Fatal("unreachable preview still shown")
	}
}
