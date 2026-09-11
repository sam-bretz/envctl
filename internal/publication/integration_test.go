package publication

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealGitHubNonPublishingReadiness(t *testing.T) {
	if os.Getenv("ENVCTL_GITHUB_PROBE_TEST") != "1" {
		t.Skip("requires explicitly selected GitHub repository and source pin")
	}
	name, pin := os.Getenv("ENVCTL_GITHUB_REPOSITORY"), os.Getenv("ENVCTL_GITHUB_SOURCE_PIN")
	if !repositoryName.MatchString(name) || !sha.MatchString(pin) {
		t.Fatal("explicit repository and immutable source pin required")
	}
	c, err := workflow.Parse([]byte("version: 2\nproject: probe\nrepositories: [{id: app, url: https://github.com/" + name + ".git, ref: main}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("Readiness probe", "Verify non-publishing GitHub readiness", "test", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.Current().SourcePins = map[string]string{"app": pin}
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := New(store)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	a := engine.Assignment{Run: *run, Revision: *run.Current()}
	passed, detail, err := b.Probe(ctx, a, "publication.pr")
	if err != nil || !passed {
		t.Fatal(detail, err)
	}
	branch := workflow.OutputBranch(c.Repositories[0], a.Revision.ID)
	head, err := b.branch(ctx, name, branch)
	if err != nil || head != "" {
		t.Fatal("readiness created a remote branch", err)
	}
	t.Log("GitHub repository permission, base/source commits, PR lookup and Git receive-pack dry run verified; no branch or PR created")
}
