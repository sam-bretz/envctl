package engine

import "testing"

func TestAReportedModelReplacesTheConfiguredOneOnTheAttempt(t *testing.T) {
	// A role with nothing configured is exactly the case the configuration
	// cannot answer, so what the harness reports has to win.
	got := newModels(map[string]string{"supervisor": "opus"}, map[string]string{"worker": "claude-sonnet-5-20260115"})
	if got["worker"] != "claude-sonnet-5-20260115" || got["supervisor"] != "opus" {
		t.Fatalf("merge lost a role: %v", got)
	}
	// An alias resolves to its version.
	got = newModels(map[string]string{"worker": "sonnet"}, map[string]string{"worker": "claude-sonnet-5-20260115"})
	if got["worker"] != "claude-sonnet-5-20260115" {
		t.Fatalf("alias not replaced by the version that ran: %v", got)
	}
	// Nothing new means no write, so an unchanged attempt is not rewritten.
	if got = newModels(map[string]string{"worker": "opus"}, map[string]string{"worker": "opus"}); got != nil {
		t.Fatalf("rewrote an unchanged attempt: %v", got)
	}
	if got = newModels(map[string]string{"worker": "opus"}, nil); got != nil {
		t.Fatalf("rewrote when the harness said nothing: %v", got)
	}
}
