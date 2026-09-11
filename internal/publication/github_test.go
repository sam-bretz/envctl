package publication

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type hosting struct {
	bare       string
	base       string
	push       bool
	prs        []pull
	posts      int
	loseCreate bool
	// Submodule repositories other than team/app, and the order in which PRs
	// were created across all repositories.
	modules map[string]*moduleHost
	created []string
}

func TestConcurrentBranchPublicationProbesShareOneObjectStore(t *testing.T) {
	b, h, a, result := setup(t)
	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			branch := a
			branch.Child = fmt.Sprintf("branch-%d", i)
			ok, detail, err := b.Probe(context.Background(), branch, "publication.pr")
			if err != nil {
				errs <- err
			} else if !ok {
				errs <- errors.New(detail)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal("concurrent probe:", err)
	}
	errs = make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := b.Publish(context.Background(), a, result)
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal("concurrent publication replay:", err)
	}
	if h.posts != 1 {
		t.Fatal("publication effect duplicated", h.posts)
	}
	unlock, err := b.lock(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := b.Probe(ctx, a, "publication.pr"); !errors.Is(err, context.Canceled) {
		t.Fatal("publication lock ignored cancelled transport", err)
	}
}

func (h *hosting) Call(ctx context.Context, method, endpoint string, body, out any) error {
	if parts := strings.SplitN(endpoint, "/", 4); len(parts) >= 3 && parts[0] == "repos" {
		name := strings.SplitN(parts[1]+"/"+parts[2], "?", 2)[0]
		if m, ok := h.modules[name]; ok {
			return m.call(ctx, h, name, method, endpoint, body, out)
		} else if name != "team/app" {
			return HTTPError{404}
		}
	}
	var value any
	switch {
	case endpoint == "repos/team/app":
		value = map[string]any{"permissions": map[string]bool{"push": h.push}}
	case strings.Contains(endpoint, "/commits/"):
		ref, _ := url.PathUnescape(strings.SplitN(endpoint, "/commits/", 2)[1])
		if ref == "main" {
			ref = h.base
		}
		pin, err := git(ctx, h.bare, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return HTTPError{404}
		}
		value = map[string]string{"sha": pin}
	case strings.Contains(endpoint, "/git/ref/heads/"):
		ref, _ := url.PathUnescape(strings.SplitN(endpoint, "/git/ref/heads/", 2)[1])
		pin, err := git(ctx, h.bare, "rev-parse", "--verify", "refs/heads/"+ref)
		if err != nil {
			return HTTPError{404}
		}
		value = map[string]any{"object": map[string]string{"sha": pin}}
	case strings.Contains(endpoint, "/pulls?"):
		value = h.prs
		if value == nil {
			value = []pull{}
		}
	case endpoint == "repos/team/app/pulls" && method == "POST":
		raw, _ := json.Marshal(body)
		var req struct {
			Head, Base, Body string
			Draft            bool
		}
		_ = json.Unmarshal(raw, &req)
		pin, err := git(ctx, h.bare, "rev-parse", "refs/heads/"+req.Head)
		if err != nil {
			return err
		}
		h.posts++
		h.created = append(h.created, "team/app")
		p := pull{Number: h.posts, URL: fmt.Sprintf("https://github.com/team/app/pull/%d", h.posts), Body: req.Body, State: "open"}
		p.Head.Ref = req.Head
		p.Head.SHA = pin
		p.Head.Repo.FullName = "team/app"
		p.Base.Ref = req.Base
		p.Base.Repo.FullName = "team/app"
		h.prs = append(h.prs, p)
		if !req.Draft {
			return errors.New("expected draft PR")
		}
		if h.loseCreate {
			h.loseCreate = false
			return errors.New("lost response after successful PR creation")
		}
		value = p
	default:
		return fmt.Errorf("unexpected API call %s %s", method, endpoint)
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, out)
}
func localGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %s %v", args, raw, err)
	}
	return strings.TrimSpace(string(raw))
}
func setup(t *testing.T) (*Broker, *hosting, engine.Assignment, workflow.Result) {
	t.Helper()
	root := t.TempDir()
	source := filepath.Join(root, "source")
	_ = os.Mkdir(source, 0700)
	localGit(t, source, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(source, "app.txt"), []byte("before\n"), 0600)
	localGit(t, source, "add", ".")
	localGit(t, source, "-c", "user.name=fixture", "-c", "user.email=test@localhost", "commit", "-qm", "base")
	base := localGit(t, source, "rev-parse", "HEAD")
	bare := filepath.Join(root, "remote.git")
	localGit(t, root, "clone", "-q", "--bare", source, bare)
	_ = os.WriteFile(filepath.Join(source, "app.txt"), []byte("feature\n"), 0600)
	localGit(t, source, "add", ".")
	localGit(t, source, "-c", "user.name=fixture", "-c", "user.email=test@localhost", "commit", "-qm", "feature")
	commit := localGit(t, source, "rev-parse", "HEAD")
	bundle := filepath.Join(root, "source.bundle")
	localGit(t, source, "bundle", "create", bundle, "HEAD")
	store, err := runstore.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories:\n  - id: app\n    url: https://github.com/team/app.git\n    ref: main\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("Real feature", "implement feature", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.Current().State = "active"
	run.Current().SourcePins = map[string]string{"app": base}
	raw, _ := os.ReadFile(bundle)
	src, err := store.PutArtifact("source", "application/x-git-bundle", raw)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := store.PutArtifact("change", "text/markdown", []byte("feature implemented and tested"))
	if err != nil {
		t.Fatal(err)
	}
	result := workflow.Result{Summary: "Feature implemented and tested", Artifacts: []workflow.Artifact{doc}, Commits: map[string]string{"app": commit}, Sources: map[string]workflow.Artifact{"app": src}}
	result.Review = workflow.Review{Accepted: true, Summary: "reviewed", EvidenceDigest: doc.Digest, ResultDigest: result.WorkDigest()}
	run.Current().Checkpoints["qa"] = workflow.Checkpoint{ID: "cp_qa", Result: workflow.Result{Commits: workflow.Clone(result.Commits)}}
	a := engine.Assignment{Run: *run, Revision: *run.Current(), Attempt: workflow.Attempt{ID: "attempt_publish", Node: "approved-change", State: "verifying", Result: &result, Approval: &workflow.Approval{Actor: "test", At: time.Now(), ResultDigest: result.WorkDigest()}}}
	h := &hosting{bare: bare, base: base, push: true, prs: []pull{}}
	b := &Broker{Store: store, API: h, Remote: func(string) string { return bare }}
	return b, h, a, result
}
func TestPublicationReconcilesLostAcknowledgementAndNeverOverwrites(t *testing.T) {
	b, h, a, r := setup(t)
	ctx := context.Background()
	passed, detail, err := b.Probe(ctx, a, "publication.pr")
	if err != nil || !passed {
		t.Fatal(detail, err)
	}
	branch := workflow.OutputBranch(a.Revision.Config.Repositories[0], a.Revision.ID)
	if head, err := b.branch(ctx, "team/app", branch); err != nil || head != "" {
		t.Fatal("probe mutated remote", err)
	}
	h.loseCreate = true
	if _, err = b.Publish(ctx, a, r); err == nil {
		t.Fatal("lost response was not exposed")
	}
	fresh := &Broker{Store: b.Store, API: h, Remote: b.Remote}
	urls, err := fresh.Publish(ctx, a, r)
	if err != nil || urls["app"] != "https://github.com/team/app/pull/1" || h.posts != 1 {
		t.Fatal("publication duplicated after reconnect", urls, err, h.posts)
	}
	localGit(t, h.bare, "update-ref", "refs/heads/"+branch, h.base)
	if _, err = fresh.Publish(ctx, a, r); err == nil {
		t.Fatal("changed remote branch accepted")
	}
	if got := localGit(t, h.bare, "rev-parse", "refs/heads/"+branch); got != h.base {
		t.Fatal("foreign branch edit overwritten")
	}
}
func TestPublicationRequiresReadinessApprovalAndExactQA(t *testing.T) {
	b, h, a, r := setup(t)
	ctx := context.Background()
	if _, err := b.Publish(ctx, a, r); err == nil {
		t.Fatal("published without destination readiness lock")
	}
	h.push = false
	if passed, _, _ := b.Probe(ctx, a, "publication.pr"); passed {
		t.Fatal("missing push permission passed")
	}
	h.push = true
	if passed, detail, _ := b.Probe(ctx, a, "publication.pr"); !passed {
		t.Fatal(detail)
	}
	a.Attempt.Approval = nil
	if _, err := b.Publish(ctx, a, r); err == nil {
		t.Fatal("published without approval")
	}
	a.Attempt.Approval = &workflow.Approval{Actor: "test", ResultDigest: r.WorkDigest()}
	qa := a.Revision.Checkpoints["qa"]
	qa.Result.Commits["app"] = h.base
	a.Revision.Checkpoints["qa"] = qa
	if _, err := b.Publish(ctx, a, r); err == nil {
		t.Fatal("published different commits from QA")
	}
	if h.posts != 0 {
		t.Fatal("rejected candidate reached external publication")
	}
}
func TestBaseDriftAndExistingPRMismatchRequireNewPlan(t *testing.T) {
	b, h, a, r := setup(t)
	ctx := context.Background()
	if ok, d, _ := b.Probe(ctx, a, "publication.pr"); !ok {
		t.Fatal(d)
	}
	// Candidate becomes available in the hosting object store, simulating a
	// teammate moving the target branch after readiness was locked.
	dir, _ := b.dir(a, "app")
	raw, _ := b.Store.Artifact(r.Sources["app"].Digest)
	file := filepath.Join(dir, "candidate.bundle")
	_ = os.WriteFile(file, raw, 0600)
	localGit(t, h.bare, "fetch", "-q", file, r.Commits["app"])
	h.base = r.Commits["app"]
	if ok, _, _ := b.Probe(ctx, a, "publication.pr"); ok {
		t.Fatal("base drift silently renewed readiness")
	}
	if _, err := b.Publish(ctx, a, r); err == nil {
		t.Fatal("published after target base drift")
	}
}
func TestDestinationAndRefBoundaries(t *testing.T) {
	for _, ref := range []string{"main:other", "--delete", "foo/../bar", "foo.lock", "a//b", "refs/heads/.hidden", "main\nother"} {
		if validRef(ref) {
			t.Fatal("unsafe branch", ref)
		}
	}
	for _, raw := range []string{"https://secret@github.com/team/app.git", "https://github.com.evil/team/app", "/local/repo", "https://github.com/team/app?token=x"} {
		if _, err := target(workflow.Repository{URL: raw, Ref: "main"}); err == nil {
			t.Fatal("unsafe/ambiguous destination", raw)
		}
	}
	if _, err := target(workflow.Repository{URL: "ssh://git@github.com/team/app.git", Ref: "main"}); err != nil {
		t.Fatal(err)
	}
}

func TestUnchangedDependencyNeedsNoArtificialPR(t *testing.T) {
	b, h, a, r := setup(t)
	ctx := context.Background()
	a.Revision.Config.Repositories = append(a.Revision.Config.Repositories, workflow.Repository{ID: "dependency", URL: "/local/read-only", Ref: "HEAD"})
	node := a.Revision.Config.Workflow.Nodes["code"]
	node.Writes = []string{"app"}
	a.Revision.Config.Workflow.Nodes["code"] = node
	a.Revision.SourcePins["dependency"] = h.base
	r.Commits["dependency"] = h.base
	r.Sources["dependency"] = r.Sources["app"]
	r.Review.ResultDigest = r.WorkDigest()
	a.Attempt.Result = &r
	a.Attempt.Approval.ResultDigest = r.WorkDigest()
	qa := a.Revision.Checkpoints["qa"]
	qa.Result.Commits = workflow.Clone(r.Commits)
	a.Revision.Checkpoints["qa"] = qa
	if ok, d, _ := b.Probe(ctx, a, "publication.pr"); !ok {
		t.Fatal(d)
	}
	urls, err := b.Publish(ctx, a, r)
	if err != nil || len(urls) != 1 || urls["app"] == "" {
		t.Fatal("unchanged dependency blocked publication", urls, err)
	}
	r.PRs = urls
	a.Attempt.Result = &r
	a.Revision.Attempts = []workflow.Attempt{a.Attempt}
	if _, err = a.Revision.Accept(a.Attempt.ID, time.Now()); err != nil {
		t.Fatal("unchanged dependency blocked checkpoint", err)
	}
}
