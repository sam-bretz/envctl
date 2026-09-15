package secrets

import (
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestValuesAndRedactCoverHarnessAndTrackerCredentials(t *testing.T) {
	t.Setenv("ENVCTL_SECRETS_TEST_HARNESS", "harness-secret-value")
	t.Setenv("ENVCTL_SECRETS_TEST_TRACKER", "tracker-secret-value")

	cfg := workflow.Config{
		Agents: workflow.AgentConfig{
			Worker:     workflow.Harness{Kind: "claude", Credential: "env:ENVCTL_SECRETS_TEST_HARNESS"},
			Supervisor: workflow.Harness{Kind: "claude", Credential: "env:ENVCTL_SECRETS_TEST_HARNESS"},
		},
		Tracker: &workflow.TrackerConfig{Provider: "linear", Credential: "env:ENVCTL_SECRETS_TEST_TRACKER"},
	}

	values := Values(cfg)
	foundHarness, foundTracker := false, false
	for _, v := range values {
		if v == "harness-secret-value" {
			foundHarness = true
		}
		if v == "tracker-secret-value" {
			foundTracker = true
		}
	}
	if !foundHarness {
		t.Fatal("harness credential missing from Values()")
	}
	if !foundTracker {
		t.Fatal("tracker credential missing from Values()")
	}

	body := "the log contains harness-secret-value and tracker-secret-value inline"
	redacted := string(Redact(cfg, []byte(body)))
	if strings.Contains(redacted, "harness-secret-value") || strings.Contains(redacted, "tracker-secret-value") {
		t.Fatalf("redaction left a secret in place: %q", redacted)
	}
	if strings.Count(redacted, "[REDACTED]") != 2 {
		t.Fatalf("expected two redactions, got: %q", redacted)
	}
}

func TestValuesSkipsUnresolvableCredentials(t *testing.T) {
	cfg := workflow.Config{Tracker: &workflow.TrackerConfig{Provider: "linear", Credential: "env:ENVCTL_SECRETS_TEST_MISSING_VAR"}}
	if values := Values(cfg); len(values) != 0 {
		t.Fatalf("expected no values for an unresolvable credential, got %v", values)
	}
}
