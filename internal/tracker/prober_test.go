package tracker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func fixtureAssignment(t *testing.T, cfg *workflow.TrackerConfig, taskRef string) engine.Assignment {
	t.Helper()
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	c.Tracker = cfg
	if cfg != nil {
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	run, err := workflow.NewRun("demo", "objective", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.TaskRef = taskRef
	return engine.Assignment{Run: *run, Revision: *run.Current()}
}

func TestProberRoutesOnlyTrackerComment(t *testing.T) {
	ok, detail, err := LinearProber{}.Probe(context.Background(), fixtureAssignment(t, nil, ""), "publication.pr")
	if ok || err != nil || detail != "capability has no prepared invocation binding" {
		t.Fatalf("unexpected pass-through: %v %q %v", ok, detail, err)
	}
}

func TestProberNoTrackerConfigured(t *testing.T) {
	ok, detail, err := LinearProber{}.Probe(context.Background(), fixtureAssignment(t, nil, "ENG-1"), "tracker.comment")
	if ok || err != nil || !strings.Contains(detail, "no tracker configured") {
		t.Fatalf("unexpected result: %v %q %v", ok, detail, err)
	}
}

func TestProberNoTaskRef(t *testing.T) {
	cfg := &workflow.TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN_TEST"}
	ok, detail, err := LinearProber{}.Probe(context.Background(), fixtureAssignment(t, cfg, ""), "tracker.comment")
	if ok || err != nil || !strings.Contains(detail, "no linked tracker issue") {
		t.Fatalf("unexpected result: %v %q %v", ok, detail, err)
	}
}

func TestProberInvalidRef(t *testing.T) {
	cfg := &workflow.TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN_TEST"}
	ok, detail, err := LinearProber{}.Probe(context.Background(), fixtureAssignment(t, cfg, "not-a-ref"), "tracker.comment")
	if ok || err != nil || !strings.Contains(detail, "is not a Linear issue URL or identifier") {
		t.Fatalf("unexpected result: %v %q %v", ok, detail, err)
	}
}

func TestProberCredentialUnavailable(t *testing.T) {
	cfg := &workflow.TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN_DOES_NOT_EXIST_XYZ"}
	ok, detail, err := LinearProber{}.Probe(context.Background(), fixtureAssignment(t, cfg, "ENG-1"), "tracker.comment")
	if ok || err != nil || !strings.Contains(detail, "credential unavailable") {
		t.Fatalf("unexpected result: %v %q %v", ok, detail, err)
	}
}
