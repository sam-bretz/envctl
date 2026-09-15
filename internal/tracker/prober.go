package tracker

import (
	"context"
	"strings"

	"github.com/sam-bretz/envctl/internal/engine"
)

// LinearProber implements Prober for the tracker.comment readiness
// capability: it verifies the credential, the linked issue's existence, and
// comment permission using only read-only GraphQL queries, never posting.
type LinearProber struct{}

var _ Prober = LinearProber{}

func (LinearProber) Probe(ctx context.Context, a engine.Assignment, capability string) (bool, string, error) {
	if capability != "tracker.comment" {
		return false, "capability has no prepared invocation binding", nil
	}
	cfg := a.Revision.Config.Tracker
	if cfg == nil {
		return false, "no tracker configured for this invocation", nil
	}
	if strings.TrimSpace(a.Run.TaskRef) == "" {
		return false, "run has no linked tracker issue", nil
	}
	ref, err := ParseRef(a.Run.TaskRef)
	if err != nil {
		return false, err.Error(), nil
	}
	client, err := newLinearClient(*cfg)
	if err != nil {
		return false, "tracker credential unavailable", nil
	}
	viewerID, err := client.viewer(ctx)
	if err != nil {
		return false, "tracker credential rejected", nil
	}
	_, teamID, err := client.issue(ctx, ref)
	if err != nil {
		return false, "linked issue is not reachable: " + ref, nil
	}
	ok, err := client.canComment(ctx, teamID, viewerID)
	if err != nil {
		return false, "could not verify comment permission", nil
	}
	if !ok {
		return false, "credential lacks comment permission on the linked issue's team", nil
	}
	return true, "Linear credential, issue and comment permission verified", nil
}
