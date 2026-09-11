package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/gueststack"
)

// Dataset seed/restore/capture operations have stable IDs, so a recreated
// coordinator reconnects to the same guest job. A proven terminal failure
// cannot be retried under that ID: the guest journal keeps its result. The
// receipt below advances a durable generation instead, mirroring plugin
// terminal recovery. Transport loss never advances it.
type dataRecovery struct {
	Operation  string        `json:"operation"`
	Generation int           `json:"generation"`
	Failures   []dataFailure `json:"failures,omitempty"`
	RetryAt    time.Time     `json:"retry_at,omitzero"`
}

type dataFailure = pluginFailure

var errDataRecoveryExhausted = errors.New("dataset operation exhausted its configured recovery attempts; rewind or amend Plan to continue")
var errDataRecoveryBackoff = errors.New("dataset operation is waiting for its recovery backoff")

func (b *Backend) dataRecoveryPath(a engine.Assignment, operation string) (string, error) {
	dir, err := b.dir(a)
	if err != nil {
		return "", err
	}
	if !idPattern.MatchString(operation) {
		return "", errors.New("invalid dataset operation identity")
	}
	return filepath.Join(dir, "data-operations", operation+".json"), nil
}

func (b *Backend) readDataRecovery(a engine.Assignment, operation string) (dataRecovery, string, error) {
	filename, err := b.dataRecoveryPath(a, operation)
	if err != nil {
		return dataRecovery{}, "", err
	}
	record := dataRecovery{Operation: operation}
	raw, err := os.ReadFile(filename)
	if errors.Is(err, os.ErrNotExist) {
		return record, filename, nil
	}
	if err != nil {
		return record, filename, err
	}
	if json.Unmarshal(raw, &record) != nil || record.Operation != operation || record.Generation < 0 {
		return record, filename, errors.New("invalid dataset recovery receipt")
	}
	return record, filename, nil
}

// dataOperation runs one dataset action at its current recovery generation.
func (b *Backend) dataOperation(ctx context.Context, a engine.Assignment, p gueststack.Prepared, operation string, run func(checkpoint.Client) error) error {
	record, filename, err := b.readDataRecovery(a, operation)
	if err != nil {
		return err
	}
	limit := a.Revision.Config.Limits.MaxAttempts
	if limit <= 0 {
		limit = 3
	}
	if len(record.Failures) >= limit {
		return errDataRecoveryExhausted
	}
	if time.Now().Before(record.RetryAt) {
		return errDataRecoveryBackoff
	}
	adapter := b.dataAdapter(a, p)
	adapter.Generation = record.Generation
	err = run(adapter)
	var terminal *checkpoint.TerminalFailure
	if !errors.As(err, &terminal) {
		return err
	}
	failure := dataFailure{Generation: record.Generation, At: time.Now().UTC(), Detail: terminal.Error()}
	// Evidence passes through the credential redactor; data jobs carry no
	// harness secrets, but the journal tail is still untrusted guest output.
	failure.Evidence, err = b.artifact(a, "dataset.failure", "application/json", mustJSON(map[string]any{"operation": operation, "generation": record.Generation, "state": terminal.State, "detail": terminal.Detail, "output_tail": terminal.Output, "terminal": true, "at": failure.At}))
	if err != nil {
		return err
	}
	record.Failures = append(record.Failures, failure)
	record.Generation++
	record.RetryAt = failure.At.Add(time.Second * time.Duration(1<<min(record.Generation, 6)))
	if err = atomicJSON(filename, record); err != nil {
		return err
	}
	return terminal
}
