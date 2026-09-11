package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRunShowSeparatesHostPreviewFromGuestEndpoints(t *testing.T) {
	var out bytes.Buffer
	printRuntime(&out, "", workflow.RuntimeState{PreviewURL: "http://127.0.0.1:41234/", Services: []workflow.Service{{Name: "web", State: "running/healthy", URL: "http://127.0.0.1:18089"}, {Name: "db", State: "running"}}})
	printRuntime(&out, "left", workflow.RuntimeState{PreviewURL: "http://127.0.0.1:41235/"})
	got := out.String()
	for _, want := range []string{
		"  preview: http://127.0.0.1:41234/\n",
		"  service web: running/healthy, inside the VM at http://127.0.0.1:18089\n",
		"  service db: running\n",
		"  preview (left branch): http://127.0.0.1:41235/\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("run show output missing %q:\n%s", want, got)
		}
	}
}
