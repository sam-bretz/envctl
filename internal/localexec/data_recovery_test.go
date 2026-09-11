package localexec

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/gueststack"
)

func TestDataTerminalRecoveryGenerationsBackoffAndBudget(t *testing.T) {
	b, a := fixture(t)
	a.Revision.Config.Limits.MaxAttempts = 2
	t.Setenv("ENVCTL_DATA_REDACTION", "data-fixture-secret")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_DATA_REDACTION"
	ctx := context.Background()
	p := gueststack.Prepared{}
	op := "attempt_one_restore_billing"
	var seen []int
	fail := func(c checkpoint.Client) error {
		seen = append(seen, c.Generation)
		return &checkpoint.TerminalFailure{State: "interrupted", Detail: "guest job ended without a verified result", Output: "restore log mentions data-fixture-secret"}
	}
	var terminal *checkpoint.TerminalFailure
	if err := b.dataOperation(ctx, a, p, op, fail); !errors.As(err, &terminal) {
		t.Fatal("terminal failure not surfaced", err)
	}
	record, filename, err := b.readDataRecovery(a, op)
	if err != nil || record.Generation != 1 || len(record.Failures) != 1 || !record.RetryAt.After(time.Now()) {
		t.Fatal("terminal failure did not advance a durable generation with backoff", record, err)
	}
	evidence, err := b.Store.Artifact(record.Failures[0].Evidence.Digest)
	if err != nil || strings.Contains(string(evidence), "data-fixture-secret") || !strings.Contains(string(evidence), "interrupted") {
		t.Fatalf("failure evidence missing or unredacted: %s %v", evidence, err)
	}
	if err = b.dataOperation(ctx, a, p, op, fail); !errors.Is(err, errDataRecoveryBackoff) || len(seen) != 1 {
		t.Fatal("backoff did not hold the next generation", err, seen)
	}
	expire := func() {
		t.Helper()
		r, _, err := b.readDataRecovery(a, op)
		if err != nil {
			t.Fatal(err)
		}
		r.RetryAt = time.Now().Add(-time.Second)
		if err = atomicJSON(filename, r); err != nil {
			t.Fatal(err)
		}
	}
	expire()
	// A recreated coordinator resumes at the durable generation. Transport
	// loss reconnects to that generation's job instead of advancing it.
	b = New(b.Store)
	transport := func(c checkpoint.Client) error {
		seen = append(seen, c.Generation)
		return errors.New("guest job status transport failed")
	}
	if err = b.dataOperation(ctx, a, p, op, transport); err == nil || errors.As(err, &terminal) {
		t.Fatal("transport loss was treated as terminal", err)
	}
	if record, _, _ = b.readDataRecovery(a, op); record.Generation != 1 || len(record.Failures) != 1 {
		t.Fatal("transport loss advanced the generation", record)
	}
	if err = b.dataOperation(ctx, a, p, op, fail); !errors.As(err, &terminal) {
		t.Fatal(err)
	}
	expire()
	if err = b.dataOperation(ctx, a, p, op, fail); !errors.Is(err, errDataRecoveryExhausted) {
		t.Fatal("exhausted budget did not become explicit recovery work", err)
	}
	if want := []int{0, 1, 1}; len(seen) != len(want) || seen[0] != 0 || seen[1] != 1 || seen[2] != 1 {
		t.Fatal("unexpected generations", seen)
	}
	// Other operations keep their own identity and budget.
	if err = b.dataOperation(ctx, a, p, "attempt_one_capture_billing", func(c checkpoint.Client) error {
		if c.Generation != 0 {
			return errors.New("independent operation inherited another generation")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filename); err != nil {
		t.Fatal(err)
	}
}
