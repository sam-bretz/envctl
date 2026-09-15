// Package secrets collects credential values that must never reach agent
// prompts, artifacts, or tracker comments, and redacts them from raw bytes.
// It is a leaf package shared by internal/localexec and internal/tracker so
// both paths apply identical redaction.
package secrets

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Values collects every credential value resolvable from cfg: harness,
// plugin and tracker credentials. Resolution failures are skipped rather
// than surfaced, since redaction must never fail a caller that only wants to
// scrub what happens to be available.
func Values(cfg workflow.Config) []string {
	var values []string
	for _, h := range []workflow.Harness{cfg.Agents.Worker, cfg.Agents.Supervisor} {
		if c, err := agent.ResolveCredential(cfg.Dir, h.Kind, h.Credential); err == nil {
			values = append(values, c.Secrets...)
		}
	}
	for _, ref := range cfg.Plugins {
		if credentials, err := plugin.Credentials(cfg.Dir, plugin.Binding{Ref: ref}); err == nil {
			for _, value := range credentials {
				values = append(values, value)
			}
		}
	}
	if cfg.Tracker != nil {
		if v, err := resolveTrackerCredential(cfg.Tracker.Credential); err == nil {
			values = append(values, v)
		}
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	return values
}

// resolveTrackerCredential duplicates internal/tracker.ResolveCredential's
// tiny env:/file: resolution rather than importing internal/tracker, which
// itself imports this package to redact comment bodies before delivery.
func resolveTrackerCredential(reference string) (string, error) {
	kind, path, ok := strings.Cut(reference, ":")
	if !ok {
		return "", os.ErrInvalid
	}
	switch kind {
	case "env":
		v, ok := os.LookupEnv(path)
		if !ok || v == "" {
			return "", os.ErrNotExist
		}
		return v, nil
	case "file":
		raw, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		v := strings.TrimSpace(string(raw))
		if v == "" {
			return "", os.ErrNotExist
		}
		return v, nil
	default:
		return "", os.ErrInvalid
	}
}

// Redact replaces every credential value resolvable from cfg with
// "[REDACTED]" in raw, both as a literal byte string and as it would appear
// JSON-escaped inside a string value.
func Redact(cfg workflow.Config, raw []byte) []byte {
	for _, secret := range Values(cfg) {
		if secret == "" {
			continue
		}
		escaped, _ := json.Marshal(secret)
		raw = bytes.ReplaceAll(raw, escaped[1:len(escaped)-1], []byte("[REDACTED]"))
		raw = bytes.ReplaceAll(raw, []byte(secret), []byte("[REDACTED]"))
	}
	return raw
}
