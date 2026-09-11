package workflow

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) Config {
	t.Helper()
	c, err := Parse([]byte(`version: 2
project: test
repositories:
 - id: app
   url: /source/app
workflow:
 template: feature
`))
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func live(t *testing.T) *Run {
	t.Helper()
	r, err := NewRun("test", "Build feature", "dev", fixture(t), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.Current().State = "active"
	r.Current().Runtime = RuntimeState{ID: "vm-one", Ready: true}
	return r
}
func ready(rev *Revision, now time.Time) {
	for _, c := range rev.Requirements() {
		rev.SetProbe(Probe{Capability: c, Binding: "builtin@1", Passed: true, ConfigDigest: Digest(rev.Config), RuntimeID: rev.Runtime.ID, EvidenceDigest: Digest(c), CheckedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Hour)})
	}
}
func output(rev *Revision, node string) Result {
	r := Result{Summary: "Evidence of completed work", Commits: map[string]string{"app": strings.Repeat("a", 40)}}
	for _, name := range rev.Config.Workflow.Nodes[node].Outputs {
		r.Artifacts = append(r.Artifacts, Artifact{Name: name, Digest: Digest(name), Size: 10})
	}
	if rev.Config.Workflow.Nodes[node].Kind == "qa" {
		r.Checks = []CheckResult{{Name: "unit", Passed: true, EvidenceDigest: Digest("test log"), CommitsDigest: Digest(r.Commits)}}
	}
	review(&r)
	return r
}
func review(r *Result) {
	r.Review = Review{Accepted: true, Summary: "Reviewed against inputs", EvidenceDigest: Digest("review log"), ResultDigest: r.WorkDigest()}
}
func finish(t *testing.T, r *Run, node string) Checkpoint {
	t.Helper()
	rev := r.Current()
	a, err := rev.Begin(node, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res := output(rev, node)
	if err = rev.Propose(a.ID, res, time.Now()); err != nil {
		t.Fatal(err)
	}
	cp, err := rev.Accept(a.ID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return cp
}
func TestStrictConfigAndTemplateOverrides(t *testing.T) {
	c := fixture(t)
	if len(c.Workflow.Nodes) != 6 || c.Workflow.Nodes["approved-change"].Gate != "human" {
		t.Fatal("default feature contract missing")
	}
	raw := `version: 2
project: test
repositories: [{id: app, url: /source}]
workflow:
 template: feature
 nodes:
  approved-change:
   gate: ""
`
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if c.Workflow.Nodes["approved-change"].Gate != "" {
		t.Fatal("explicit empty override lost")
	}
	for _, bad := range []string{raw + "typo: true\n", raw + "---\nproject: other\n", strings.Replace(raw, "gate:", "gatte:", 1), strings.Replace(raw, "version: 2", "version: 9", 1)} {
		if _, err = Parse([]byte(bad)); err == nil {
			t.Errorf("accepted invalid config %s", bad)
		}
	}
}

func TestSourceBundlesAreBoundToReviewAndCompleteCommitSet(t *testing.T) {
	r := live(t)
	v := r.Current()
	result := output(v, "task")
	result.Sources = map[string]Artifact{"app": {Name: "source", MediaType: "application/x-git-bundle", Digest: Digest("bundle"), Size: 20}}
	review(&result)
	if err := v.validateResult("task", result, time.Now()); err != nil {
		t.Fatal(err)
	}
	changed := result.Sources["app"]
	changed.Digest = Digest("different bundle")
	result.Sources["app"] = changed
	if err := v.validateResult("task", result, time.Now()); err == nil {
		t.Fatal("changed source bundle retained old review")
	}
	review(&result)
	result.Sources["unknown"] = changed
	if err := v.validateResult("task", result, time.Now()); err == nil {
		t.Fatal("source checkpoint with unknown repository accepted")
	}
}

func TestRewindPreservesPinsUntilSourceInputsChange(t *testing.T) {
	r := live(t)
	base := strings.Repeat("a", 40)
	r.Current().SourcePins = map[string]string{"app": base}
	if _, err := r.Rewind("task", "revised objective", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.Current().SourcePins["app"] != base {
		t.Fatal("rewind lost the immutable source pin")
	}
	c := Clone(r.Current().Config)
	c.Repositories[0].Ref = "different-branch"
	if _, err := r.Rewind("task", "", &c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.Current().SourcePins["app"] != "" {
		t.Fatal("explicit ref change reused an unrelated source pin")
	}
	c = Clone(r.Current().Config)
	c.Repositories[0].BaseSHA = strings.Repeat("b", 40)
	if _, err := r.Rewind("task", "", &c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.Current().SourcePins["app"] != c.Repositories[0].BaseSHA {
		t.Fatal("explicit new pin was ignored")
	}
}
func TestLegacyManifestConversion(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "compose.yaml"), []byte("services: {}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "envctl.yaml"), []byte("version: 1\nproject: old\nstack:\n  files: [compose.yaml]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(d)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 2 || c.Repositories[0].URL != d || c.Stack.Files[0] != "compose.yaml" {
		t.Fatal(c)
	}
}
func TestDAGValidationAndStableOrder(t *testing.T) {
	d := Feature()
	n := d.Nodes["design"]
	n.Needs = []string{"qa"}
	d.Nodes["design"] = n
	if d.Validate() == nil {
		t.Fatal("cycle accepted")
	}
	d = Feature()
	n = d.Nodes["design"]
	n.Needs = []string{"task"}
	d.Nodes["design"] = n
	if d.Validate() == nil {
		t.Fatal("Plan bypass accepted")
	}
	d = Feature()
	d.Nodes["audit"] = Node{Kind: "custom", Needs: []string{"design"}, Outputs: []string{"audit"}}
	n = d.Nodes["qa"]
	n.Needs = append(n.Needs, "audit")
	d.Nodes["qa"] = n
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
	first, _ := d.Order()
	for range 100 {
		next, _ := d.Order()
		if strings.Join(first, ",") != strings.Join(next, ",") {
			t.Fatal("unstable order")
		}
	}
}
func TestPlanCannotAdvanceFromAgentClaim(t *testing.T) {
	r := live(t)
	finish(t, r, "task")
	rev := r.Current()
	a, err := rev.Begin("plan", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = rev.Propose(a.ID, output(rev, "plan"), time.Now()); err == nil || !strings.Contains(err.Error(), "readiness") {
		t.Fatalf("missing readiness admitted: %v", err)
	}
	if _, err = rev.Begin("design", time.Now()); err == nil {
		t.Fatal("downstream dispatched without Plan")
	}
	ready(rev, time.Now())
	if err = rev.Propose(a.ID, output(rev, "plan"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = rev.Accept(a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	rev.Readiness[0].ExpiresAt = time.Now().Add(-time.Second)
	if _, err = rev.Begin("design", time.Now()); err == nil {
		t.Fatal("expired readiness admitted")
	}
	ready(rev, time.Now())
	rev.Runtime.ID = "vm-other"
	if len(rev.ReadinessProblems(time.Now(), rev.Requirements())) == 0 {
		t.Fatal("different VM reused probes")
	}
}
func TestCheckpointsRequireArtifactsAndBoundReview(t *testing.T) {
	r := live(t)
	rev := r.Current()
	a, _ := rev.Begin("task", time.Now())
	res := output(rev, "task")
	res.Artifacts = nil
	review(&res)
	if rev.Propose(a.ID, res, time.Now()) == nil {
		t.Fatal("missing artifact accepted")
	}
	res = output(rev, "task")
	res.Summary = "changed after review"
	if rev.Propose(a.ID, res, time.Now()) == nil {
		t.Fatal("stale supervisor review accepted")
	}
}
func TestQARequiresPassingTestsOnSameCommits(t *testing.T) {
	r := live(t)
	ready(r.Current(), time.Now())
	for _, n := range []string{"task", "plan", "design", "code"} {
		finish(t, r, n)
	}
	rev := r.Current()
	a, _ := rev.Begin("qa", time.Now())
	res := output(rev, "qa")
	res.Checks[0].CommitsDigest = Digest("different code")
	review(&res)
	if rev.Propose(a.ID, res, time.Now()) == nil {
		t.Fatal("tests for wrong commits accepted")
	}
	res = output(rev, "qa")
	res.Checks = nil
	review(&res)
	if rev.Propose(a.ID, res, time.Now()) == nil {
		t.Fatal("QA without executed evidence accepted")
	}
}
func TestApprovalMatchesExactCandidate(t *testing.T) {
	r := live(t)
	ready(r.Current(), time.Now())
	for _, n := range []string{"task", "plan", "design", "code", "qa"} {
		finish(t, r, n)
	}
	rev := r.Current()
	a, _ := rev.Begin("approved-change", time.Now())
	res := output(rev, "approved-change")
	if err := rev.Propose(a.ID, res, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := rev.Accept(a.ID, time.Now()); err == nil {
		t.Fatal("human gate bypassed")
	}
	if r.Approve(rev.ID, a.ID, "dev", "wrong", time.Now()) == nil {
		t.Fatal("wrong work approved")
	}
	if err := r.Approve(rev.ID, a.ID, "dev", res.WorkDigest(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := rev.Accept(a.ID, time.Now()); err == nil {
		t.Fatal("PR output missing")
	}
	a.Result.PRs = map[string]string{"app": "https://example.com/pull/1"}
	if _, err := rev.Accept(a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if rev.State != "completed" {
		t.Fatal(rev.State)
	}
}
func TestRewindDrainsAndInvalidatesDescendants(t *testing.T) {
	r := live(t)
	ready(r.Current(), time.Now())
	for _, n := range []string{"task", "plan", "design"} {
		finish(t, r, n)
	}
	old := r.Current()
	oldID := old.ID
	task := old.Checkpoints["task"]
	a, _ := old.Begin("code", time.Now())
	attemptID := a.ID
	id, err := r.Rewind("plan", "", nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if id == oldID || r.Revision(oldID).State != "draining" {
		t.Fatal("not isolated revision")
	}
	if len(r.Current().Checkpoints) != 1 || r.Current().Checkpoints["task"].ID != task.ID {
		t.Fatal("wrong invalidation")
	}
	old = r.Revision(oldID)
	if _, err = old.Begin("qa", time.Now()); err == nil {
		t.Fatal("superseded revision dispatched")
	}
	if err = old.Propose(attemptID, output(old, "code"), time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = old.Accept(attemptID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if old.State != "superseded" {
		t.Fatal("drained run still active")
	}
	if _, ok := r.Current().Checkpoints["code"]; ok {
		t.Fatal("old output leaked")
	}
	queued := r.CurrentRevision
	if _, err = r.Rewind("task", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if r.Revision(queued).State != "superseded" {
		t.Fatal("queued revision resurrectable")
	}
}
func TestPluginChangeReopensPlan(t *testing.T) {
	r := live(t)
	ready(r.Current(), time.Now())
	for _, n := range []string{"task", "plan", "design"} {
		finish(t, r, n)
	}
	c := Clone(r.Current().Config)
	c.Plugins = []PluginRef{{ID: "browser", Source: "./browser", Version: "1"}}
	if _, err := r.Rewind("code", "", &c, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok := r.Current().Checkpoints["plan"]; ok {
		t.Fatal("plugin mutation reused Plan")
	}
	if len(r.Current().Readiness) > 0 {
		t.Fatal("old readiness leaked")
	}
}
func TestRetryAndCancelRetainFailureHistory(t *testing.T) {
	r := live(t)
	rev := r.Current()
	a, _ := rev.Begin("task", time.Now())
	if err := rev.Fail(a.ID, "crash", time.Now()); err != nil {
		t.Fatal(err)
	}
	a2, err := rev.Begin("task", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if a2.Number != 2 {
		t.Fatal(a2.Number)
	}
	r.Cancel(time.Now())
	if rev.Attempts[0].State != "failed" || rev.Attempts[1].State != "cancelled" {
		t.Fatal(rev.Attempts)
	}
}

func TestCannotApproveUntestedCommits(t *testing.T) {
	r := live(t)
	ready(r.Current(), time.Now())
	for _, n := range []string{"task", "plan", "design", "code", "qa"} {
		finish(t, r, n)
	}
	rev := r.Current()
	a, _ := rev.Begin("approved-change", time.Now())
	res := output(rev, "approved-change")
	res.Commits["app"] = strings.Repeat("b", 40)
	review(&res)
	if rev.Propose(a.ID, res, time.Now()) == nil {
		t.Fatal("untested commits accepted for publication")
	}
}
func TestBuiltinReadinessCannotBeRemovedByTemplateOverride(t *testing.T) {
	r := live(t)
	for id, n := range r.Current().Config.Workflow.Nodes {
		n.Requires = nil
		r.Current().Config.Workflow.Nodes[id] = n
	}
	if len(r.Current().Requirements()) < 5 {
		t.Fatal("mandatory execution capabilities omitted")
	}
}
