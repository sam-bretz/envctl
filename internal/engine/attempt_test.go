package engine

import (
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestTheIssueMovesToPublishedOnlyWhenAPullRequestWasOpened(t *testing.T) {
	tracked := func(t *testing.T) (*workflow.Run, *workflow.Revision) {
		t.Helper()
		c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
		if err != nil {
			t.Fatal(err)
		}
		r, err := workflow.NewRun("demo", "ship it", "dev", c, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		r.TaskRef = "ENG-1"
		r.Current().Config.Tracker = &workflow.TrackerConfig{Provider: "linear", Credential: "env:LINEAR_API_KEY",
			Mapping: map[string]workflow.TrackerStatusMapping{workflow.DefaultWorkflow: {PRPublished: "Done"}}}
		return r, r.Current()
	}
	kinds := func(r *workflow.Run) map[string]bool {
		out := map[string]bool{}
		for _, e := range r.TrackerLog {
			out[e.Kind] = true
		}
		return out
	}

	// A change that opened nothing must not tell the tracker it is done.
	r, v := tracked(t)
	logPublication(r, v, "approved-change", "attempt_1", nil, time.Now())
	if got := kinds(r); got[workflow.TrackerEventPRPublished] || got[workflow.TrackerKindPublished] {
		t.Fatalf("an unpublished change moved the issue or logged a pull request: %v", got)
	}

	// The positive control: a real pull request does both, comment first.
	r, v = tracked(t)
	logPublication(r, v, "approved-change", "attempt_1", map[string]string{"app": "https://github.com/o/r/pull/7"}, time.Now())
	if got := kinds(r); !got[workflow.TrackerEventPRPublished] || !got[workflow.TrackerKindPublished] {
		t.Fatalf("a published change did not both comment and move the issue: %v", got)
	}
	if r.TrackerLog[0].Kind != workflow.TrackerKindPublished || r.TrackerLog[1].StatusName != "Done" {
		t.Fatalf("expected the comment then the move to Done, got %+v", r.TrackerLog)
	}
}
