package tracker

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func renderFixture(t *testing.T) (*workflow.Run, *workflow.Revision) {
	t.Helper()
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("demo", "Ship the thing", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.TaskRef = "ENG-1"
	return run, run.Current()
}

func markReady(rev *workflow.Revision, now time.Time) {
	for _, c := range rev.Requirements() {
		rev.SetProbe(workflow.Probe{Capability: c, Binding: "builtin@1", Passed: true, ConfigDigest: workflow.Digest(rev.Config), RuntimeID: rev.Runtime.ID, EvidenceDigest: workflow.Digest(c), CheckedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)})
	}
}

func acceptCheckpoint(t *testing.T, rev *workflow.Revision, node string, mutate func(*workflow.Result)) workflow.Checkpoint {
	t.Helper()
	if rev.Config.Workflow.Nodes[node].Kind == "plan" {
		markReady(rev, time.Now())
	}
	a, err := rev.Begin(node, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res := workflow.Result{Summary: "Did the work", Commits: map[string]string{"app": strings.Repeat("a", 40)}}
	for _, name := range rev.Config.Workflow.Nodes[node].Outputs {
		res.Artifacts = append(res.Artifacts, workflow.Artifact{Name: name, Digest: workflow.Digest(name), Size: 10})
	}
	if rev.Config.Workflow.Nodes[node].Kind == "qa" {
		res.Checks = []workflow.CheckResult{{Name: "unit", Passed: true, EvidenceDigest: workflow.Digest("log"), CommitsDigest: workflow.Digest(res.Commits)}}
	}
	if mutate != nil {
		mutate(&res)
	}
	res.Review = workflow.Review{Accepted: true, Summary: "Looks good", EvidenceDigest: workflow.Digest("review"), ResultDigest: res.WorkDigest()}
	if err = rev.Propose(a.ID, res, time.Now()); err != nil {
		t.Fatal(err)
	}
	cp, err := rev.Accept(a.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestRenderStageCompleted(t *testing.T) {
	run, rev := renderFixture(t)
	rev.State = "active"
	rev.Runtime = workflow.RuntimeState{ID: "vm", Ready: true}
	cp := acceptCheckpoint(t, rev, "task", nil)

	entry := workflow.TrackerLogEntry{ID: "tlog_1", Kind: workflow.TrackerKindStageCompleted, Revision: rev.ID, Node: "task", Attempt: cp.Attempt}
	body, err := Render(run, entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## task — attempt 1", "**Summary**", "Did the work", "**Usage so far**"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestRenderStageCompletedWithChecksCommitsAndPRs(t *testing.T) {
	run, rev := renderFixture(t)
	rev.State = "active"
	rev.Runtime = workflow.RuntimeState{ID: "vm", Ready: true}
	acceptCheckpoint(t, rev, "task", nil)
	rev.Config.Data = workflow.DataConfig{}
	cp := acceptCheckpoint(t, rev, "plan", func(r *workflow.Result) {})
	_ = cp

	// Directly craft a change-like checkpoint with checks, commits and PRs to
	// exercise every optional section without threading the full DAG.
	rev.Checkpoints["design"] = workflow.Checkpoint{
		ID: "cp_x", Node: "design", Attempt: "attempt_x",
		Result: workflow.Result{
			Summary: "Designed it",
			Review:  workflow.Review{Summary: "Reviewed the design"},
			Checks:  []workflow.CheckResult{{Name: "unit", Passed: true}, {Name: "lint", Passed: false, ExitCode: 1}},
			Commits: map[string]string{"app": strings.Repeat("b", 40)},
			PRs:     map[string]string{"app": "https://github.com/acme/app/pull/1"},
		},
	}
	rev.Attempts = append(rev.Attempts, workflow.Attempt{ID: "attempt_x", Node: "design", Number: 2})

	entry := workflow.TrackerLogEntry{ID: "tlog_2", Kind: workflow.TrackerKindStageCompleted, Revision: rev.ID, Node: "design", Attempt: "attempt_x"}
	body, err := Render(run, entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"## design — attempt 2",
		"**Review**\nReviewed the design",
		"`unit`: pass (exit 0)",
		"`lint`: fail (exit 1)",
		"- app: `" + strings.Repeat("b", 40) + "`",
		"- app: https://github.com/acme/app/pull/1",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestRenderSoFarCoversOnlyEarlierPostedEntries(t *testing.T) {
	run, rev := renderFixture(t)
	rev.State = "active"
	rev.Runtime = workflow.RuntimeState{ID: "vm", Ready: true}
	acceptCheckpoint(t, rev, "task", func(r *workflow.Result) { r.Summary = "Task summary line" })
	acceptCheckpoint(t, rev, "plan", nil)

	run.TrackerLog = []workflow.TrackerLogEntry{
		{ID: "tlog_task", Kind: workflow.TrackerKindStageCompleted, Revision: rev.ID, Node: "task", Status: "posted"},
		{ID: "tlog_plan_pending", Kind: workflow.TrackerKindStageCompleted, Revision: rev.ID, Node: "plan", Status: "pending"},
		{ID: "tlog_design", Kind: workflow.TrackerKindStageCompleted, Revision: rev.ID, Node: "design"},
	}
	body := soFar(run, run.TrackerLog[2])
	if !strings.Contains(body, "`task`: Task summary line") {
		t.Fatalf("so-far section missing posted task entry:\n%s", body)
	}
	if strings.Contains(body, "`plan`:") {
		t.Fatalf("so-far section included a non-posted entry:\n%s", body)
	}
}

func TestRenderAwaitingApprovalAndApproved(t *testing.T) {
	run, rev := renderFixture(t)
	rev.State = "active"
	rev.Runtime = workflow.RuntimeState{ID: "vm", Ready: true}
	n := rev.Config.Workflow.Nodes["approved-change"]
	n.Gate = "human"
	rev.Config.Workflow.Nodes["approved-change"] = n
	rev.Attempts = append(rev.Attempts, workflow.Attempt{
		ID: "att_1", Node: "approved-change", Number: 1, State: "awaiting-approval",
		Result: &workflow.Result{Summary: "Ready for review"},
	})
	entry := workflow.TrackerLogEntry{Kind: workflow.TrackerKindAwaitingApproval, Revision: rev.ID, Node: "approved-change", Attempt: "att_1"}
	body, err := Render(run, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "awaiting human approval") || !strings.Contains(body, "Ready for review") {
		t.Fatalf("unexpected awaiting-approval body: %s", body)
	}

	rev.Attempts[len(rev.Attempts)-1].Approval = &workflow.Approval{Actor: "alice"}
	entry.Kind = workflow.TrackerKindApproved
	body, err = Render(run, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "approved by alice") {
		t.Fatalf("unexpected approved body: %s", body)
	}
}

func TestRenderRewoundAndNeedsAttention(t *testing.T) {
	run, rev := renderFixture(t)
	body, err := Render(run, workflow.TrackerLogEntry{Kind: workflow.TrackerKindRewound, Revision: rev.ID, Detail: "target plan; objective changed"})
	if err != nil || !strings.Contains(body, "New revision") || !strings.Contains(body, "objective changed") {
		t.Fatalf("unexpected rewound body: %q, %v", body, err)
	}
	body, err = Render(run, workflow.TrackerLogEntry{Kind: workflow.TrackerKindNeedsAttention, Revision: rev.ID, Detail: "token ceiling reached"})
	if err != nil || !strings.Contains(body, "Needs attention: token ceiling reached") {
		t.Fatalf("unexpected needs_attention body: %q, %v", body, err)
	}
}

func TestThePullRequestIsItsOwnLogEntry(t *testing.T) {
	run, rev := renderFixture(t)
	// Craft the change checkpoint directly rather than threading the DAG; the
	// entry only needs the published result.
	set := func(prs map[string]string) {
		rev.Checkpoints["approved-change"] = workflow.Checkpoint{
			ID: "cp_change", Node: "approved-change", Attempt: "attempt_change",
			Result: workflow.Result{Summary: "Opened the change", PRs: prs},
		}
	}
	set(map[string]string{"app": "https://github.com/o/r/pull/7"})

	entry := workflow.TrackerLogEntry{Kind: workflow.TrackerKindPublished, Revision: rev.ID, Node: "approved-change"}
	body, err := Render(run, entry)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "https://github.com/o/r/pull/7") {
		t.Fatalf("the PR URL is the point of this entry: %q", body)
	}

	// A run spanning repositories names each one.
	set(map[string]string{"app": "https://github.com/o/r/pull/7", "web": "https://github.com/o/w/pull/3"})
	if body, err = Render(run, entry); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"`app`", "`web`", "pull/7", "pull/3"} {
		if !strings.Contains(body, want) {
			t.Fatalf("missing %s in:\n%s", want, body)
		}
	}

	// Nothing published is a bug in the caller, not an empty comment.
	set(nil)
	if _, err = Render(run, entry); err == nil {
		t.Fatal("rendered a published entry with no pull request")
	}
}
