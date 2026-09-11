package guestjob

import (
	"io"
	"os/exec"
	"strings"
	"testing"
)

func TestProtocolResponseBound(t *testing.T) {
	b := boundedOutput{limit: 10}
	if _, err := io.Copy(&b, strings.NewReader(strings.Repeat("x", 20))); err == nil {
		t.Fatal("unbounded response accepted")
	}
	if b.buffer.Len() > 10 {
		t.Fatal("response cap bypassed")
	}
}

func TestGuestJournalProtocol(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python 3 is required for guest journal tests")
	}
	cmd := exec.Command(python, "-B", "runner_test.py", "-v")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("guest journal: %v\n%s", err, b)
	}
}
