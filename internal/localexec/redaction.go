package localexec

import (
	"bytes"
	"errors"
	"strings"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/secrets"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func agentSecrets(a engine.Assignment) []string { return secrets.Values(a.Revision.Config) }
func clean(a engine.Assignment, raw []byte) []byte {
	return secrets.Redact(a.Revision.Config, raw)
}
func (b *Backend) artifact(a engine.Assignment, name, media string, raw []byte) (workflow.Artifact, error) {
	if strings.HasPrefix(media, "text/") || media == "application/json" {
		raw = clean(a, raw)
	} else {
		for _, secret := range agentSecrets(a) {
			if secret != "" && bytes.Contains(raw, []byte(secret)) {
				return workflow.Artifact{}, errors.New("source checkpoint contains a harness credential; remove it before checkpointing")
			}
		}
	}
	return b.Store.PutArtifact(name, media, raw)
}
