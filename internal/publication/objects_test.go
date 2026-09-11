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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// moduleHost fakes the GitHub API for one submodule repository backed by a
// real bare repository.
type moduleHost struct {
	bare       string
	push       bool
	prs        []pull
	posts      int
	calls      int
	loseCreate bool
}

func (m *moduleHost) call(ctx context.Context, h *hosting, name, method, endpoint string, body, out any) error {
	m.calls++
	var value any
	switch {
	case endpoint == "repos/"+name:
		value = map[string]any{"default_branch": "main", "permissions": map[string]bool{"push": m.push}}
	case strings.Contains(endpoint, "/commits/"):
		ref, _ := url.PathUnescape(strings.SplitN(endpoint, "/commits/", 2)[1])
		pin, err := git(ctx, m.bare, "rev-parse", "--verify", ref+"^{commit}")
		if err != nil {
			return HTTPError{422}
		}
		value = map[string]string{"sha": pin}
	case strings.Contains(endpoint, "/git/ref/heads/"):
		ref, _ := url.PathUnescape(strings.SplitN(endpoint, "/git/ref/heads/", 2)[1])
		pin, err := git(ctx, m.bare, "rev-parse", "--verify", "refs/heads/"+ref)
		if err != nil {
			return HTTPError{404}
		}
		value = map[string]any{"object": map[string]string{"sha": pin}}
	case strings.Contains(endpoint, "/pulls?"):
		value = m.prs
		if value == nil {
			value = []pull{}
		}
	case endpoint == "repos/"+name+"/pulls" && method == "POST":
		raw, _ := json.Marshal(body)
		var req struct {
			Head, Base, Body string
			Draft            bool
		}
		_ = json.Unmarshal(raw, &req)
		pin, err := git(ctx, m.bare, "rev-parse", "refs/heads/"+req.Head)
		if err != nil {
			return err
		}
		m.posts++
		h.created = append(h.created, name)
		p := pull{Number: m.posts, URL: fmt.Sprintf("https://github.com/%s/pull/%d", name, m.posts), Body: req.Body, State: "open"}
		p.Head.Ref, p.Head.SHA, p.Head.Repo.FullName = req.Head, pin, name
		p.Base.Ref, p.Base.Repo.FullName = req.Base, name
		m.prs = append(m.prs, p)
		if !req.Draft {
			return errors.New("expected draft PR")
		}
		if m.loseCreate {
			m.loseCreate = false
			return errors.New("lost response after successful PR creation")
		}
		value = p
	default:
		return fmt.Errorf("unexpected API call %s %s", method, endpoint)
	}
	raw, _ := json.Marshal(value)
	return json.Unmarshal(raw, out)
}

type moduleFixture struct {
	b                  *Broker
	h                  *hosting
	a                  engine.Assignment
	r                  workflow.Result
	app, lib, leaf     string // bare remotes
	libWork, leafWork  string // submodule checkouts inside the root worktree
	appOID, libOID     string
	libCommit, leafNew string
}

func commitAll(t *testing.T, dir, message string) string {
	t.Helper()
	localGit(t, dir, "add", "-A")
	localGit(t, dir, "-c", "user.name=fixture", "-c", "user.email=test@localhost", "commit", "-qm", message)
	return localGit(t, dir, "rev-parse", "HEAD")
}

func useLFS(t *testing.T, dir string) {
	t.Helper()
	for _, kv := range [][2]string{{"filter.lfs.process", "git-lfs filter-process"}, {"filter.lfs.required", "true"}, {"filter.lfs.clean", "git-lfs clean -- %f"}, {"filter.lfs.smudge", "git-lfs smudge -- %f"}} {
		localGit(t, dir, "config", kv[0], kv[1])
	}
}

func write(t *testing.T, filename, body string) {
	t.Helper()
	if err := os.WriteFile(filename, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func oidOf(t *testing.T, dir, file string) string {
	t.Helper()
	var doc struct {
		Files []struct{ Name, OID string }
	}
	if err := json.Unmarshal([]byte(localGit(t, dir, "lfs", "ls-files", "--json", "HEAD")), &doc); err != nil {
		t.Fatal(err)
	}
	for _, f := range doc.Files {
		if f.Name == file {
			return f.OID
		}
	}
	t.Fatal("LFS object missing for", file)
	return ""
}

// newModuleFixture builds team/app with an LFS file and submodule vendor/lib
// (relative URL ../lib.git), which nests submodule leaf (../leaf.git). The
// approved change adds LFS content to app and lib and, when changeModules is
// set, new commits in lib and leaf that no remote has. The companion comes
// from the real guest capture program.
func newModuleFixture(t *testing.T, changeModules bool) *moduleFixture {
	t.Helper()
	for _, tool := range []string{"git-lfs", "python3"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " is required for submodule/LFS publication fixtures")
		}
	}
	root := t.TempDir()
	f := &moduleFixture{app: filepath.Join(root, "app.git"), lib: filepath.Join(root, "lib.git"), leaf: filepath.Join(root, "leaf.git")}
	for _, bare := range []string{f.app, f.lib, f.leaf} {
		localGit(t, root, "init", "-q", "--bare", "-b", "main", bare)
	}
	source := func(name, remote string) string {
		dir := filepath.Join(root, name)
		localGit(t, root, "init", "-q", "-b", "main", dir)
		localGit(t, dir, "remote", "add", "origin", remote)
		return dir
	}
	leaf := source("leaf-src", f.leaf)
	write(t, filepath.Join(leaf, "leaf.txt"), "leaf base\n")
	commitAll(t, leaf, "leaf base")
	localGit(t, leaf, "push", "-q", "origin", "main")
	lib := source("lib-src", f.lib)
	write(t, filepath.Join(lib, "lib.txt"), "lib base\n")
	write(t, filepath.Join(lib, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	localGit(t, lib, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "../leaf.git", "leaf")
	commitAll(t, lib, "lib base")
	localGit(t, lib, "push", "-q", "origin", "main")
	app := source("app-src", f.app)
	write(t, filepath.Join(app, "app.txt"), "before\n")
	write(t, filepath.Join(app, ".gitattributes"), "*.bin filter=lfs diff=lfs merge=lfs -text\n")
	localGit(t, app, "-c", "protocol.file.allow=always", "submodule", "add", "-q", "../lib.git", "vendor/lib")
	base := commitAll(t, app, "app base")
	localGit(t, app, "push", "-q", "origin", "main")
	localGit(t, app, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init", "--recursive")
	f.libWork, f.leafWork = filepath.Join(app, "vendor", "lib"), filepath.Join(app, "vendor", "lib", "leaf")
	if changeModules {
		write(t, filepath.Join(f.leafWork, "leaf.txt"), "leaf feature\n")
		f.leafNew = commitAll(t, f.leafWork, "leaf feature")
		useLFS(t, f.libWork)
		write(t, filepath.Join(f.libWork, "lib.bin"), "library binary\x00")
		f.libCommit = commitAll(t, f.libWork, "lib feature")
		f.libOID = oidOf(t, f.libWork, "lib.bin")
	}
	useLFS(t, app)
	write(t, filepath.Join(app, "app.txt"), "feature\n")
	write(t, filepath.Join(app, "app.bin"), "application binary\x00")
	commit := commitAll(t, app, "feature")
	f.appOID = oidOf(t, app, "app.bin")
	program, err := os.ReadFile(filepath.Join("..", "repository", "objects.py"))
	if err != nil {
		t.Fatal(err)
	}
	companionFile := filepath.Join(root, "objects.tar")
	if out, err := exec.Command("python3", "-c", string(program), "capture", app, commit, companionFile).CombinedOutput(); err != nil {
		t.Fatalf("guest capture program: %v\n%s", err, out)
	}
	bundle := filepath.Join(root, "source.bundle")
	localGit(t, app, "bundle", "create", "-q", bundle, "HEAD")
	store, err := runstore.Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories:\n  - id: app\n    url: https://github.com/team/app.git\n    ref: main\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("Module feature", "implement feature", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.Current().State = "active"
	run.Current().SourcePins = map[string]string{"app": base}
	put := func(name, media, filename string) workflow.Artifact {
		raw, err := os.ReadFile(filename)
		if err != nil {
			t.Fatal(err)
		}
		artifact, err := store.PutArtifact(name, media, raw)
		if err != nil {
			t.Fatal(err)
		}
		return artifact
	}
	doc, err := store.PutArtifact("change", "text/markdown", []byte("feature implemented and tested"))
	if err != nil {
		t.Fatal(err)
	}
	f.r = workflow.Result{Summary: "Feature with submodule and LFS content", Artifacts: []workflow.Artifact{doc}, Commits: map[string]string{"app": commit},
		Sources:       map[string]workflow.Artifact{"app": put("source", "application/x-git-bundle", bundle)},
		SourceObjects: map[string]workflow.Artifact{"app": put("source-objects", repository.ObjectsMediaType, companionFile)}}
	f.a = engine.Assignment{Run: *run, Revision: *run.Current(), Attempt: workflow.Attempt{ID: "attempt_publish", Node: "approved-change", State: "verifying"}}
	f.rebind()
	f.h = &hosting{bare: f.app, base: base, push: true, prs: []pull{}, modules: map[string]*moduleHost{"team/lib": {bare: f.lib, push: true}, "team/leaf": {bare: f.leaf, push: true}}}
	remotes := map[string]string{"team/app": f.app, "team/lib": f.lib, "team/leaf": f.leaf}
	f.b = &Broker{Store: store, API: f.h, Remote: func(name string) string { return "file://" + remotes[name] }}
	return f
}

// rebind makes the approval, review and QA evidence match the current result.
func (f *moduleFixture) rebind() {
	f.r.Review = workflow.Review{Accepted: true, Summary: "reviewed", EvidenceDigest: f.r.Artifacts[0].Digest}
	f.r.Review.ResultDigest = f.r.WorkDigest()
	r := f.r
	f.a.Attempt.Result = &r
	f.a.Attempt.Approval = &workflow.Approval{Actor: "test", At: time.Now(), ResultDigest: r.WorkDigest()}
	f.a.Revision.Checkpoints["qa"] = workflow.Checkpoint{ID: "cp_qa", Result: workflow.Result{Commits: workflow.Clone(r.Commits)}}
}

func (f *moduleFixture) fresh() *Broker {
	return &Broker{Store: f.b.Store, API: f.h, Remote: f.b.Remote}
}

func hasLFS(bare, oid string) bool {
	_, err := os.Stat(filepath.Join(bare, "lfs", "objects", oid[:2], oid[2:4], oid))
	return err == nil
}

func branches(t *testing.T, bare string) []string {
	t.Helper()
	out := localGit(t, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	var names []string
	for _, name := range strings.Split(out, "\n") {
		if name != "" && name != "main" {
			names = append(names, name)
		}
	}
	return names
}

func TestPublicationPublishesSubmodulesAndLFSChildrenFirst(t *testing.T) {
	f := newModuleFixture(t, true)
	ctx := context.Background()
	if ok, detail, err := f.b.Probe(ctx, f.a, "publication.pr"); err != nil || !ok {
		t.Fatal(detail, err)
	}
	// A lost PR-create acknowledgement in the middle submodule.
	f.h.modules["team/lib"].loseCreate = true
	if _, err := f.b.Publish(ctx, f.a, f.r); err == nil {
		t.Fatal("lost submodule PR acknowledgement was not exposed")
	}
	if len(branches(t, f.app)) != 0 {
		t.Fatal("parent branch published before its submodules")
	}
	// The root's LFS upload fails: the root branch must not be pushed.
	lfsDir := filepath.Join(f.app, "lfs")
	if err := os.MkdirAll(lfsDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(lfsDir, 0500); err != nil {
		t.Fatal(err)
	}
	_, err := f.fresh().Publish(ctx, f.a, f.r)
	_ = os.Chmod(lfsDir, 0700)
	if err == nil || !strings.Contains(err.Error(), "LFS upload") {
		t.Fatal("root LFS upload failure not exposed", err)
	}
	if len(branches(t, f.app)) != 0 || hasLFS(f.app, f.appOID) {
		t.Fatal("root branch pushed before its LFS objects were uploaded")
	}
	urls, err := f.fresh().Publish(ctx, f.a, f.r)
	if err != nil {
		t.Fatal(err)
	}
	lib, leaf := f.h.modules["team/lib"], f.h.modules["team/leaf"]
	if lib.posts != 1 || leaf.posts != 1 || f.h.posts != 1 {
		t.Fatal("publication effects duplicated", lib.posts, leaf.posts, f.h.posts)
	}
	if !slices.Equal(f.h.created, []string{"team/leaf", "team/lib", "team/app"}) {
		t.Fatal("PRs not created children first", f.h.created)
	}
	want := map[string]string{"app": "https://github.com/team/app/pull/1", "app/vendor/lib": "https://github.com/team/lib/pull/1", "app/vendor/lib/leaf": "https://github.com/team/leaf/pull/1"}
	if fmt.Sprint(urls) != fmt.Sprint(want) {
		t.Fatal("unexpected publication map", urls)
	}
	if !hasLFS(f.app, f.appOID) || !hasLFS(f.lib, f.libOID) {
		t.Fatal("LFS objects not uploaded to their own repositories")
	}
	for bare, commit := range map[string]string{f.lib: f.libCommit, f.leaf: f.leafNew} {
		heads := branches(t, bare)
		if len(heads) != 1 || localGit(t, bare, "rev-parse", "refs/heads/"+heads[0]) != commit {
			t.Fatal("submodule commit not on a revision branch", bare, heads)
		}
	}
	body := f.h.prs[0].Body
	if !strings.Contains(body, "merge these first") || !strings.Contains(body, want["app/vendor/lib"]) || !strings.Contains(body, want["app/vendor/lib/leaf"]) {
		t.Fatal("parent PR does not list its submodule PRs", body)
	}
	if !strings.Contains(lib.prs[0].Body, want["app/vendor/lib/leaf"]) {
		t.Fatal("submodule PR does not list its nested submodule PR")
	}
	// Replay after success changes nothing.
	again, err := f.fresh().Publish(ctx, f.a, f.r)
	if err != nil || fmt.Sprint(again) != fmt.Sprint(want) || lib.posts != 1 || leaf.posts != 1 || f.h.posts != 1 {
		t.Fatal("replay after success was not a no-op", again, err)
	}
	// The map keeps its repository-ID contract for checkpoint acceptance.
	r := f.r
	r.PRs = urls
	f.a.Attempt.Result = &r
	f.a.Revision.Attempts = []workflow.Attempt{f.a.Attempt}
	if _, err = f.a.Revision.Accept(f.a.Attempt.ID, time.Now()); err != nil {
		t.Fatal("submodule PR keys broke checkpoint acceptance", err)
	}
}

func TestPublicationSkipsSubmodulesTheDestinationHasOrDidNotChange(t *testing.T) {
	ctx := context.Background()
	present := newModuleFixture(t, true)
	localGit(t, present.libWork, "push", "-q", present.lib, "HEAD:refs/heads/upstream")
	localGit(t, present.leafWork, "push", "-q", present.leaf, "HEAD:refs/heads/upstream")
	if ok, detail, _ := present.b.Probe(ctx, present.a, "publication.pr"); !ok {
		t.Fatal(detail)
	}
	urls, err := present.b.Publish(ctx, present.a, present.r)
	if err != nil || len(urls) != 1 || urls["app"] == "" || present.h.modules["team/lib"].posts != 0 || present.h.modules["team/leaf"].posts != 0 {
		t.Fatal("published submodule commits the destination already has", urls, err)
	}
	unchanged := newModuleFixture(t, false)
	if ok, detail, _ := unchanged.b.Probe(ctx, unchanged.a, "publication.pr"); !ok {
		t.Fatal(detail)
	}
	if urls, err = unchanged.b.Publish(ctx, unchanged.a, unchanged.r); err != nil || len(urls) != 1 || !hasLFS(unchanged.app, unchanged.appOID) {
		t.Fatal(urls, err)
	}
	if calls := unchanged.h.modules["team/lib"].calls + unchanged.h.modules["team/leaf"].calls; calls != 0 {
		t.Fatal("unchanged submodules were queried", calls)
	}
}

func TestPublicationRefusesUnverifiableSubmoduleAndLFSSources(t *testing.T) {
	ctx := context.Background()
	f := newModuleFixture(t, true)
	if ok, detail, _ := f.b.Probe(ctx, f.a, "publication.pr"); !ok {
		t.Fatal(detail)
	}
	raw, err := f.b.Store.Artifact(f.r.SourceObjects["app"].Digest)
	if err != nil {
		t.Fatal(err)
	}
	original := f.r.SourceObjects["app"]
	// A damaged payload fails its checksum; a manifest naming a different
	// submodule commit fails the committed gitlink and bundle checks.
	for needle, want := range map[string]string{"library binary": "companion", f.libCommit: "submodule vendor/lib"} {
		i := strings.Index(string(raw), needle)
		corrupt := slices.Clone(raw)
		if needle == f.libCommit {
			// Stay a valid hex SHA: flipping a bit of 'a' or 'f' leaves hex and
			// trips an earlier format check instead of the gitlink check.
			if corrupt[i] == '0' {
				corrupt[i] = '1'
			} else {
				corrupt[i] = '0'
			}
		} else {
			corrupt[i] ^= 1
		}
		artifact, err := f.b.Store.PutArtifact("source-objects", repository.ObjectsMediaType, corrupt)
		if err != nil {
			t.Fatal(err)
		}
		f.r.SourceObjects["app"] = artifact
		f.rebind()
		if _, err = f.b.Publish(ctx, f.a, f.r); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal("corrupt companion accepted", needle, err)
		}
	}
	// Without any companion, a moved gitlink cannot be published.
	delete(f.r.SourceObjects, "app")
	f.rebind()
	if _, err = f.b.Publish(ctx, f.a, f.r); err == nil || !strings.Contains(err.Error(), "companion") {
		t.Fatal("changed submodule published without a companion", err)
	}
	if f.h.posts+f.h.modules["team/lib"].posts+f.h.modules["team/leaf"].posts != 0 || len(branches(t, f.app))+len(branches(t, f.lib))+len(branches(t, f.leaf)) != 0 {
		t.Fatal("refused publication reached a remote")
	}
	// Without git-lfs on the coordinator, Plan and publication fail explicitly
	// before any external effect.
	f.r.SourceObjects["app"] = original
	f.rebind()
	real, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	write(t, filepath.Join(bin, "git"), "#!/bin/sh\nexec '"+real+"' \"$@\"\n")
	if err = os.Chmod(filepath.Join(bin, "git"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	if lfsAvailable(ctx, bin) == nil {
		t.Skip("git-lfs is reachable through git's own exec path")
	}
	if ok, detail, _ := f.fresh().Probe(ctx, f.a, "publication.pr"); ok || !strings.Contains(detail, "git-lfs") {
		t.Fatal("Plan readiness ignored missing git-lfs for an LFS repository", detail)
	}
	if _, err = f.fresh().Publish(ctx, f.a, f.r); err == nil || !strings.Contains(err.Error(), "git-lfs") {
		t.Fatal("missing git-lfs was not explicit", err)
	}
	if f.h.posts+f.h.modules["team/lib"].posts+f.h.modules["team/leaf"].posts != 0 || len(branches(t, f.leaf)) != 0 {
		t.Fatal("publication without git-lfs reached a remote")
	}
}

func TestModuleRepositoryResolution(t *testing.T) {
	for raw, want := range map[string]string{"../lib.git": "team/lib", "../lib": "team/lib", "./../lib.git": "team/lib", "../../other/lib.git": "other/lib", "https://github.com/x/y.git": "x/y", "git@github.com:x/y.git": "x/y"} {
		if got, err := moduleRepository("team/app", raw); err != nil || got != want {
			t.Fatal(raw, got, err)
		}
	}
	for _, raw := range []string{"./sub", "../../../x", "https://gitlab.com/x/y.git", "/local/path", "../a/b"} {
		if got, err := moduleRepository("team/app", raw); err == nil {
			t.Fatal("accepted", raw, got)
		}
	}
}
