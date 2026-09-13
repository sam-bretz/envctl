package doctor

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestExecRunnerPassesThroughExitCode(t *testing.T) {
	stdout, _, code, err := ExecRunner(context.Background(), 5*time.Second, "sh", "-c", "echo hi; exit 3")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if code != 3 {
		t.Fatalf("exit code = %d, want 3", code)
	}
	if strings.TrimSpace(stdout) != "hi" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestExecRunnerSucceeds(t *testing.T) {
	_, _, code, err := ExecRunner(context.Background(), 5*time.Second, "sh", "-c", "exit 0")
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
}

func TestExecRunnerDetectsTimeout(t *testing.T) {
	_, _, _, err := ExecRunner(context.Background(), 50*time.Millisecond, "sh", "-c", "sleep 5")
	if err == nil || !strings.Contains(err.Error(), "did not respond") {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

func TestExecRunnerMissingBinary(t *testing.T) {
	_, _, _, err := ExecRunner(context.Background(), time.Second, "envctl-doctor-nonexistent-binary")
	if err == nil {
		t.Fatal("expected an error for a missing binary")
	}
}
