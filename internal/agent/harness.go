package agent

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Harness translates agent protocols. The guest journal owns process lifetime;
// the workflow engine owns acceptance, correction, and checkpoint publication.
type Harness interface {
	Install(context.Context) error
	Start(context.Context, Invocation, Credential) (guestjob.Status, error)
	Result(context.Context, string) (json.RawMessage, error)
	Session(string) string
	// Model reports the model the harness actually ran, read from its own
	// event stream. An invocation with no model names one the harness chose,
	// which the configuration never records; empty means the stream did not
	// say.
	Model(string) string
	// Activity summarizes the native event stream for live progress display.
	Activity(string) []string
	// Usage sums the model usage the native event stream reports.
	Usage(string) workflow.Usage
}

func Select(kind string, guest guestjob.Client) (Harness, error) {
	return SelectVersion(kind, "", guest)
}

func SelectVersion(kind, version string, guest guestjob.Client) (Harness, error) {
	if version != "" && version != workflow.DefaultHarnessVersion(kind) {
		return nil, errors.New("unsupported pinned harness version")
	}
	switch kind {
	case "codex":
		return Codex{Guest: guest}, nil
	case "claude":
		return Claude{Guest: guest}, nil
	default:
		return nil, errors.New("unsupported harness; select codex or claude")
	}
}

func ResolveCredential(root, kind, reference string) (Credential, error) {
	switch kind {
	case "codex":
		return CodexCredential(root, reference)
	case "claude":
		return ClaudeCredential(root, reference)
	default:
		return Credential{}, errors.New("unsupported harness credential kind")
	}
}

func (c Codex) Session(stream string) string { return Session(stream) }
func (c Codex) Model(stream string) string   { return Model(stream) }
