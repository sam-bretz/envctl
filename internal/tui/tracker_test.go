package tui

import (
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestDashboardShowsCaptainsLogCounts(t *testing.T) {
	m := modelFixture(t)
	m.Width, m.Height = 120, 40
	if strings.Contains(m.View().Content, "Captain's log") {
		t.Fatal("captain's log row shown without any tracker log entries")
	}
	m.Runs[0].TrackerLog = []workflow.TrackerLogEntry{{Status: "pending"}, {Status: "failed"}}
	if got := m.View().Content; !strings.Contains(got, "Captain's log: 1 pending, 1 failed") {
		t.Fatalf("captain's log counts not shown:\n%s", got)
	}
}
