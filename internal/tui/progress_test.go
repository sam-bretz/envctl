package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestConversationShowsLiveProgressAndSteeringStatus(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 160, 50
	rev := m.Runs[0].Current()
	node := m.nodeID()
	now := time.Now()
	rev.Attempts = []workflow.Attempt{{ID: "attempt_one", Node: node, Number: 1, State: "running",
		Progress: &workflow.Progress{Phase: "worker", Generation: 1, Activity: []string{"ran (exit 0): pytest", "agent: fixing\x1b[2J imports"}, UpdatedAt: now},
		Steering: []workflow.Delivery{{Message: "msg_start", Role: "worker", Generation: 0, At: now}, {Message: "msg_live", Role: "worker", Generation: 1, At: now}}}}
	rev.Messages = []workflow.Message{
		{ID: "msg_start", Node: node, Recipient: "worker", Body: "use the existing client"},
		{ID: "msg_live", Node: node, Recipient: "worker", Body: "also keep the API stable"},
		{ID: "msg_waiting", Node: node, Recipient: "worker", Body: "add a changelog entry"},
	}
	view := m.View().Content
	for _, want := range []string{"Live: worker (resume 1)", "ran (exit 0): pytest", "included when " + node + " worker attempt 1 started", "delivered live to " + node + " worker attempt 1 (resume 1)", "pending live delivery to " + node + " attempt 1"} {
		if !strings.Contains(view, want) {
			t.Fatalf("conversation lacks %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "\x1b[2J") {
		t.Fatal("activity escaped terminal sanitization")
	}
	rev.Attempts[0].State = "failed"
	if view = m.View().Content; strings.Contains(view, "Live: worker") || !strings.Contains(view, "queued for the next "+node+" attempt") {
		t.Fatal("finished attempt still shows live progress or lost the waiting message:\n" + view)
	}
}
