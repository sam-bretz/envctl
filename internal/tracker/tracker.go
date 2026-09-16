// Package tracker posts a captain's log to a linked external issue tracker
// as workflow stages complete. It sits behind a small interface so a second
// provider (GitHub Issues, Jira) can be added without touching the engine,
// the delivery loop, or redaction.
package tracker

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Tracker posts one comment to an issue. Comment must be idempotent under
// marker: calling it again with the same marker must reconcile to the same
// remote comment, never create a duplicate.
type Tracker interface {
	Comment(ctx context.Context, issueRef, marker, body string) (commentID string, err error)
}

// StatusUpdater reconciles an issue to a named workflow state. Implementations
// must treat an issue already in the requested state as success; that read
// before mutation closes the crash window between a remote update and the
// durable receipt.
type StatusUpdater interface {
	SetStatus(ctx context.Context, issueRef, statusName string) error
}

// Prober implements the readiness half of a tracker: verifying credential,
// issue existence and comment permission without posting anything.
type Prober interface {
	Probe(ctx context.Context, a engine.Assignment, capability string) (bool, string, error)
}

// New selects a Tracker implementation by cfg.Provider. Config.Validate
// already rejects any provider other than "linear", so the default case
// here is unreachable in practice.
func New(cfg workflow.TrackerConfig) (Tracker, error) {
	switch cfg.Provider {
	case "linear":
		return newLinearClient(cfg)
	default:
		return nil, fmt.Errorf("unsupported tracker provider %q", cfg.Provider)
	}
}

// ResolveCredential reads a tracker's env:/file: credential. A file:
// reference must be an absolute path, already enforced by
// workflow.Config.Validate; this is the only place that reads it off disk
// or the environment.
func ResolveCredential(cfg workflow.TrackerConfig) (string, error) {
	kind, path, ok := strings.Cut(cfg.Credential, ":")
	if !ok {
		return "", errors.New("invalid tracker credential reference")
	}
	switch kind {
	case "env":
		v, ok := os.LookupEnv(path)
		if !ok || v == "" {
			return "", fmt.Errorf("tracker credential requires environment variable %s", path)
		}
		return v, nil
	case "file":
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", errors.New("tracker credential file unavailable")
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", errors.New("tracker credential file is empty")
		}
		return v, nil
	default:
		return "", errors.New("unsupported tracker credential reference")
	}
}
