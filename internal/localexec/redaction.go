package localexec

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func secrets(a engine.Assignment) []string {
	var values []string
	for _, h := range []workflow.Harness{a.Revision.Config.Agents.Worker, a.Revision.Config.Agents.Supervisor} {
		if c, err := agent.ResolveCredential(a.Revision.Config.Dir, h.Kind, h.Credential); err == nil {
			values = append(values, c.Secrets...)
		}
	}
	for _, ref := range a.Revision.Config.Plugins {
		if credentials, err := plugin.Credentials(a.Revision.Config.Dir, plugin.Binding{Ref: ref}); err == nil {
			for _, value := range credentials {
				values = append(values, value)
			}
		}
	}
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	return values
}
func clean(a engine.Assignment, raw []byte) []byte {
	for _, secret := range secrets(a) {
		if secret == "" {
			continue
		}
		escaped, _ := json.Marshal(secret)
		raw = bytes.ReplaceAll(raw, escaped[1:len(escaped)-1], []byte("[REDACTED]"))
		raw = bytes.ReplaceAll(raw, []byte(secret), []byte("[REDACTED]"))
	}
	return raw
}
func (b *Backend) artifact(a engine.Assignment, name, media string, raw []byte) (workflow.Artifact, error) {
	if strings.HasPrefix(media, "text/") || media == "application/json" {
		raw = clean(a, raw)
	} else {
		for _, secret := range secrets(a) {
			if secret != "" && bytes.Contains(raw, []byte(secret)) {
				return workflow.Artifact{}, errors.New("source checkpoint contains a harness credential; remove it before checkpointing")
			}
		}
	}
	return b.Store.PutArtifact(name, media, raw)
}
