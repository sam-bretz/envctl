// Package publication brokers Git and PR effects outside worker VMs. Branches
// are created only when absent, and every effect is reconciled by a stable
// revision identity and the exact approved result digest.
package publication

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type API interface {
	Call(context.Context, string, string, any, any) error
}
type HTTPError struct{ Code int }

func (e HTTPError) Error() string { return fmt.Sprintf("GitHub request failed (HTTP %d)", e.Code) }

type CLI struct{}
type bounded struct{ bytes.Buffer }

func (b *bounded) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("broker response exceeds limit")
	}
	return b.Buffer.Write(p)
}
func (CLI) Call(ctx context.Context, method, endpoint string, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"api", "--hostname", "github.com", "--method", method, "-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2022-11-28", endpoint}
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		args = append(args, "--input", "-")
		input = bytes.NewReader(raw)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Stdin = input
	var stdout, stderr bounded
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		for _, code := range []int{401, 403, 404, 409, 422, 429, 500, 502, 503} {
			if strings.Contains(stderr.String(), fmt.Sprintf("HTTP %d", code)) {
				return HTTPError{code}
			}
		}
		return errors.New("GitHub transport failed; check the coordinator's gh connection")
	}
	if out != nil && json.Unmarshal(stdout.Bytes(), out) != nil {
		return errors.New("invalid GitHub response")
	}
	return nil
}

type Broker struct {
	Store *runstore.Store
	API   API
	// Remote is injectable for protocol/conformance tests using a real bare
	// repository. Production always derives a canonical GitHub HTTPS URL.
	Remote func(string) string
	mu     sync.Mutex
	locks  map[string]chan struct{}
}

// Parent and child readiness share one publication destination/object store.
// Serialize that scope without serializing unrelated runs or waiting past
// coordinator shutdown. Workers still execute independently in their VMs.
func (b *Broker) lock(ctx context.Context, a engine.Assignment) (func(), error) {
	key := a.Run.ID + "/" + a.Revision.ID
	b.mu.Lock()
	if b.locks == nil {
		b.locks = map[string]chan struct{}{}
	}
	gate := b.locks[key]
	if gate == nil {
		gate = make(chan struct{}, 1)
		b.locks[key] = gate
	}
	b.mu.Unlock()
	select {
	case gate <- struct{}{}:
		return func() { <-gate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func New(store *runstore.Store) *Broker { return &Broker{Store: store, API: CLI{}} }

var repositoryName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*$`)
var identity = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,100}$`)
var sha = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func validRef(ref string) bool {
	if ref == "" || len(ref) > 240 || strings.HasPrefix(ref, "-") || strings.HasSuffix(ref, "/") || strings.Contains(ref, "..") || strings.Contains(ref, "@{") || strings.Contains(ref, "//") || strings.ContainsAny(ref, " ~^:?*[\\\x00\r\n\t") {
		return false
	}
	for _, part := range strings.Split(ref, "/") {
		if part == "" || strings.HasPrefix(part, ".") || strings.HasSuffix(part, ".") || strings.HasSuffix(part, ".lock") {
			return false
		}
	}
	return true
}
func target(repo workflow.Repository) (workflow.PublicationTarget, error) {
	if repo.Publication != nil {
		t := *repo.Publication
		if t.Provider != "github" || !repositoryName.MatchString(t.Repository) || !validRef(t.Base) {
			return t, errors.New("publication requires provider github, owner/repository, and a valid base branch")
		}
		return t, nil
	}
	name, err := githubName(repo.URL)
	if err != nil {
		return workflow.PublicationTarget{}, err
	}
	base := repo.Ref
	if base == "HEAD" || base == "" || sha.MatchString(base) {
		return workflow.PublicationTarget{}, errors.New("publication base branch must be explicit for a detached source pin")
	}
	base = strings.TrimPrefix(base, "refs/heads/")
	if !validRef(base) {
		return workflow.PublicationTarget{}, errors.New("invalid publication base branch")
	}
	return workflow.PublicationTarget{Provider: "github", Repository: name, Base: base}, nil
}

// githubName accepts only unambiguous github.com HTTPS/SSH repository URLs.
func githubName(raw string) (string, error) {
	if strings.HasPrefix(raw, "git@github.com:") {
		raw = "https://github.com/" + strings.TrimPrefix(raw, "git@github.com:")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() != "github.com" || u.Port() != "" || (u.Scheme == "https" && u.User != nil) || (u.Scheme != "https" && u.Scheme != "ssh") || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("repository needs an explicit GitHub publication destination")
	}
	name := strings.TrimSuffix(strings.Trim(u.Path, "/"), ".git")
	if !repositoryName.MatchString(name) {
		return "", errors.New("invalid GitHub source destination")
	}
	return name, nil
}
func writes(a engine.Assignment, id string) bool {
	for _, n := range a.Revision.Config.Workflow.Nodes {
		if slices.Contains(n.Writes, "*") || slices.Contains(n.Writes, id) {
			return true
		}
	}
	return false
}
func (b *Broker) dir(a engine.Assignment, repo string) (string, error) {
	for _, id := range []string{a.Run.ID, a.Revision.ID, repo} {
		if !identity.MatchString(id) {
			return "", errors.New("invalid publication identity")
		}
	}
	return filepath.Join(b.Store.Dir, "publication", a.Run.ID, a.Revision.ID, repo), nil
}

type lockedTarget struct {
	Binding   string                     `json:"binding"`
	Target    workflow.PublicationTarget `json:"target"`
	BaseSHA   string                     `json:"base_sha"`
	SourceSHA string                     `json:"source_sha"`
	Branch    string                     `json:"branch"`
}

func binding(a engine.Assignment, r workflow.Repository) string {
	return workflow.Digest(struct {
		Repository    workflow.Repository
		Pin, Revision string
	}{r, a.Revision.SourcePins[r.ID], a.Revision.ID})
}
func freeze(filename string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(filename); err == nil {
		if !bytes.Equal(old, raw) {
			return errors.New("publication receipt conflicts with immutable inputs")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".receipt-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Link(f.Name(), filename); err != nil {
		if errors.Is(err, os.ErrExist) {
			return freeze(filename, value)
		}
		return err
	}
	d, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func load(filename string, out any) error {
	raw, err := os.ReadFile(filename)
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, out) != nil {
		return errors.New("invalid publication receipt")
	}
	return nil
}
func (b *Broker) remote(name string) string {
	if b.Remote != nil {
		return b.Remote(name)
	}
	return "https://github.com/" + name + ".git"
}

// gitCommand isolates broker Git (and git-lfs, which inherits these -c
// settings) from hooks, user/system configuration and prompts; the only
// credential source is the coordinator's gh connection.
func gitCommand(ctx context.Context, dir string, args ...string) *exec.Cmd {
	args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential"}, args...)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	cmd.Stderr = io.Discard
	return cmd
}

// gitStatus reports a completed command's exit code separately from failures
// to run it, for queries whose non-zero exit is a meaningful answer.
func gitStatus(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, int, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := gitCommand(ctx, dir, args...)
	var out bounded
	cmd.Stdout = &out
	err := cmd.Run()
	if ctx.Err() != nil {
		return "", -1, ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return "", exit.ExitCode(), nil
	}
	if err != nil {
		return "", -1, errors.New("broker Git operation failed to run or exceeded its output bound")
	}
	return strings.TrimSpace(out.String()), 0, nil
}

func git(ctx context.Context, dir string, args ...string) (string, error) {
	return gitTimeout(ctx, dir, 2*time.Minute, args...)
}

func gitTimeout(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, error) {
	out, code, err := gitStatus(ctx, dir, timeout, args...)
	if err != nil {
		return "", err
	}
	if code != 0 {
		return "", errors.New("broker Git operation failed; verify source, destination and coordinator credentials")
	}
	return out, nil
}
func objectDir(ctx context.Context, dir string) (string, error) {
	p := filepath.Join(dir, "objects.git")
	if err := os.MkdirAll(p, 0700); err != nil {
		return "", err
	}
	if _, err := git(ctx, p, "init", "--bare", "--quiet"); err != nil {
		return "", err
	}
	return p, nil
}
func (b *Broker) commit(ctx context.Context, repo, ref string) (string, error) {
	var r struct {
		SHA string `json:"sha"`
	}
	err := b.API.Call(ctx, "GET", "repos/"+repo+"/commits/"+url.PathEscape(ref), nil, &r)
	if err == nil && !sha.MatchString(r.SHA) {
		err = errors.New("GitHub returned an invalid commit identity")
	}
	return r.SHA, err
}
func (b *Broker) branch(ctx context.Context, repo, ref string) (string, error) {
	var r struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	err := b.API.Call(ctx, "GET", "repos/"+repo+"/git/ref/heads/"+url.PathEscape(ref), nil, &r)
	var httpErr HTTPError
	if errors.As(err, &httpErr) && httpErr.Code == 404 {
		return "", nil
	}
	if err == nil && !sha.MatchString(r.Object.SHA) {
		err = errors.New("GitHub returned an invalid branch identity")
	}
	return r.Object.SHA, err
}

func (b *Broker) Probe(ctx context.Context, a engine.Assignment, capability string) (bool, string, error) {
	if capability != "publication.pr" {
		return false, "capability has no prepared invocation binding", nil
	}
	unlock, err := b.lock(ctx, a)
	if err != nil {
		return false, "publication verification was cancelled", err
	}
	defer unlock()
	for _, repo := range a.Revision.Config.Repositories {
		if !writes(a, repo.ID) {
			continue
		}
		if err := b.probeRepo(ctx, a, repo); err != nil {
			return false, repo.ID + ": " + err.Error(), nil
		}
	}
	return true, "GitHub destinations, base pins, PR access and non-publishing Git push checks verified", nil
}
func (b *Broker) probeRepo(ctx context.Context, a engine.Assignment, repo workflow.Repository) error {
	dest, err := target(repo)
	if err != nil {
		return err
	}
	branch := workflow.OutputBranch(repo, a.Revision.ID)
	if !validRef(branch) {
		return errors.New("invalid revision output branch")
	}
	dir, err := b.dir(a, repo.ID)
	if err != nil {
		return err
	}
	var locked lockedTarget
	err = load(filepath.Join(dir, "target.json"), &locked)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && locked.Binding != binding(a, repo) {
		return errors.New("publication binding changed; revise Plan")
	}
	var info struct {
		Archived    bool `json:"archived"`
		Disabled    bool `json:"disabled"`
		Permissions struct {
			Push bool `json:"push"`
		} `json:"permissions"`
	}
	if err = b.API.Call(ctx, "GET", "repos/"+dest.Repository, nil, &info); err != nil {
		return err
	}
	if info.Archived || info.Disabled || !info.Permissions.Push {
		return errors.New("destination is unavailable or connection lacks repository push permission")
	}
	base, err := b.commit(ctx, dest.Repository, dest.Base)
	if err != nil {
		return err
	}
	if locked.Binding != "" && locked.BaseSHA != base {
		return errors.New("destination base changed after planning; revise and rerun QA")
	}
	pin := a.Revision.SourcePins[repo.ID]
	if !sha.MatchString(pin) {
		return errors.New("repository source pin is unavailable")
	}
	if _, err = b.commit(ctx, dest.Repository, pin); err != nil {
		return errors.New("pinned source commit is not available at the publication destination")
	}
	var pulls []pull
	if err = b.API.Call(ctx, "GET", pullList(dest, branch), nil, &pulls); err != nil {
		return err
	}
	head, err := b.branch(ctx, dest.Repository, branch)
	if err != nil {
		return err
	}
	if head != "" {
		var intent publicationIntent
		if load(filepath.Join(dir, "intent.json"), &intent) != nil || intent.Commit != head || intent.Binding != binding(a, repo) {
			return errors.New("output branch already exists without a matching publication intent")
		}
	} else {
		objects, err := objectDir(ctx, dir)
		if err != nil {
			return err
		}
		if _, err = git(ctx, objects, "fetch", "--quiet", "--no-tags", b.remote(dest.Repository), pin); err != nil {
			return err
		}
		// --dry-run authenticates receive-pack without creating a branch. The
		// empty lease requires absence; neither probe nor publish overwrites.
		if _, err = git(ctx, objects, "push", "--dry-run", "--porcelain", "--force-with-lease=refs/heads/"+branch+":", b.remote(dest.Repository), pin+":refs/heads/"+branch); err != nil {
			return err
		}
		// LFS publication is foreseeable from the pinned tree. Submodule push
		// access is not required here: third-party modules the worker never
		// changes must not block Plan, and publish reports any gap explicitly.
		declared, err := lfsDeclared(ctx, objects, pin)
		if err != nil {
			return err
		}
		if declared {
			if override, err := lfsOverride(ctx, objects, pin); err != nil || override {
				return errors.Join(err, errors.New("pinned source configures a custom Git LFS endpoint in .lfsconfig; the publication broker only uploads to the GitHub destination"))
			}
			if err = lfsAvailable(ctx, objects); err != nil {
				return err
			}
		}
	}
	if locked.Binding == "" {
		locked = lockedTarget{Binding: binding(a, repo), Target: dest, BaseSHA: base, SourceSHA: pin, Branch: branch}
		return freeze(filepath.Join(dir, "target.json"), locked)
	}
	return nil
}

type publicationIntent struct{ Binding, WorkDigest, Commit, Branch string }
type pull struct {
	Number int    `json:"number"`
	URL    string `json:"html_url"`
	Body   string `json:"body"`
	State  string `json:"state"`
	Head   struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref  string `json:"ref"`
		Repo struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"base"`
}

func pullList(t workflow.PublicationTarget, branch string) string {
	q := url.Values{"state": {"all"}, "head": {strings.Split(t.Repository, "/")[0] + ":" + branch}, "base": {t.Base}, "per_page": {"100"}}
	return "repos/" + t.Repository + "/pulls?" + q.Encode()
}
func marker(a engine.Assignment, repo string, r workflow.Result) string {
	return "<!-- envctl:" + a.Run.ID + ":" + a.Revision.ID + ":" + repo + ":" + r.WorkDigest() + " -->"
}
func verified(p pull, t lockedTarget, commit, mark string) bool {
	return p.Number > 0 && p.State == "open" && p.Head.SHA == commit && p.Head.Ref == t.Branch && strings.EqualFold(p.Head.Repo.FullName, t.Target.Repository) && strings.EqualFold(p.Base.Repo.FullName, t.Target.Repository) && p.Base.Ref == t.Target.Base && strings.Contains(p.Body, mark) && p.URL == fmt.Sprintf("https://github.com/%s/pull/%d", t.Target.Repository, p.Number)
}

// Publish returns PR URLs keyed by repository ID. Draft PRs opened in
// submodule repositories for commits their destination lacked are keyed
// "<repository>/<submodule path>"; repository IDs never contain "/".
func (b *Broker) Publish(ctx context.Context, a engine.Assignment, r workflow.Result) (map[string]string, error) {
	unlock, err := b.lock(ctx, a)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if a.Run.CurrentRevision != a.Revision.ID || a.Revision.State != "active" || a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Kind != "change" || a.Attempt.State != "verifying" {
		return nil, errors.New("publication requires the current verified change stage")
	}
	if a.Attempt.Result == nil || a.Attempt.Result.WorkDigest() != r.WorkDigest() || !r.Review.Accepted || r.Review.ResultDigest != r.WorkDigest() {
		return nil, errors.New("publication result does not match reviewed work")
	}
	if err := a.Revision.ValidateResult(a.Attempt.Node, r, time.Now()); err != nil {
		return nil, fmt.Errorf("publication candidate rejected: %w", err)
	}
	if a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Gate == "human" && (a.Attempt.Approval == nil || a.Attempt.Approval.ResultDigest != r.WorkDigest()) {
		return nil, errors.New("publication requires exact-result human approval")
	}
	urls := map[string]string{}
	changed := 0
	for _, repo := range a.Revision.Config.Repositories {
		if r.Commits[repo.ID] == a.Revision.SourcePins[repo.ID] {
			continue
		}
		if !writes(a, repo.ID) {
			return nil, errors.New("publication includes an undeclared repository writer")
		}
		url, modules, err := b.publishRepo(ctx, a, repo, r)
		if err != nil {
			return nil, err
		}
		urls[repo.ID] = url
		changed++
		for _, m := range modules {
			urls[repo.ID+"/"+m.Path] = m.URL
		}
	}
	if changed == 0 {
		return nil, errors.New("approved change contains no repository changes to publish")
	}
	return urls, nil
}

// publishRepo verifies the retained source and its submodule/LFS companion
// before any external effect, then publishes children before parents: changed
// submodule commits, this repository's LFS objects, its branch, and its PR.
func (b *Broker) publishRepo(ctx context.Context, a engine.Assignment, repo workflow.Repository, r workflow.Result) (string, []modulePR, error) {
	dir, err := b.dir(a, repo.ID)
	if err != nil {
		return "", nil, err
	}
	var locked lockedTarget
	if load(filepath.Join(dir, "target.json"), &locked) != nil || locked.Binding != binding(a, repo) {
		return "", nil, errors.New("publication has no matching Plan destination lock")
	}
	base, err := b.commit(ctx, locked.Target.Repository, locked.Target.Base)
	if err != nil {
		return "", nil, err
	}
	if base != locked.BaseSHA {
		return "", nil, errors.New("destination base changed; approval must follow revalidated QA")
	}
	commit := r.Commits[repo.ID]
	artifact, ok := r.Sources[repo.ID]
	if !ok || !sha.MatchString(commit) {
		return "", nil, errors.New("publication requires a retained source bundle for each changed repository")
	}
	bundle, err := b.Store.Artifact(artifact.Digest)
	if err != nil || int64(len(bundle)) != artifact.Size {
		return "", nil, errors.New("publication source bundle is missing or corrupt")
	}
	objects, err := objectDir(ctx, dir)
	if err != nil {
		return "", nil, err
	}
	if err = fetchBundle(ctx, objects, dir, bundle, commit); err != nil {
		return "", nil, err
	}
	if _, err = git(ctx, objects, "merge-base", "--is-ancestor", locked.SourceSHA, commit); err != nil {
		return "", nil, errors.New("candidate does not descend from the planned source pin")
	}
	c, err := b.companion(ctx, objects, repo, r, locked.SourceSHA, commit)
	if err != nil {
		return "", nil, err
	}
	var modules []modulePR
	if c != nil {
		nodes, err := moduleTree(ctx, objects, dir, c, locked.SourceSHA)
		if err != nil {
			return "", nil, err
		}
		if err = verifyObjects(ctx, objects, nodes); err != nil {
			return "", nil, err
		}
		if modules, err = b.publishModules(ctx, a, repo, r, dir, objects, locked, c, nodes); err != nil {
			return "", nil, err
		}
		// The root is last in children-first order.
		if err = b.pushLFS(ctx, objects, locked.Target.Repository, c, nodes[len(nodes)-1].entry.LFS); err != nil {
			return "", nil, err
		}
	}
	intent := publicationIntent{Binding: locked.Binding, WorkDigest: r.WorkDigest(), Commit: commit, Branch: locked.Branch}
	if err = freeze(filepath.Join(dir, "intent.json"), intent); err != nil {
		return "", nil, err
	}
	if err = b.pushBranch(ctx, objects, locked, commit); err != nil {
		return "", nil, err
	}
	chosen, err := b.reconcilePR(ctx, locked, commit, marker(a, repo.ID, r), strings.TrimSpace(a.Run.Name), func(mark string) string {
		body := r.Summary + "\n\nValidated commits:\n"
		ids := make([]string, 0, len(r.Commits))
		for id := range r.Commits {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		for _, id := range ids {
			body += "- " + id + ": `" + r.Commits[id] + "`\n"
		}
		body += modulesSection(modules)
		body += "\nValidation:\n"
		nodes := make([]string, 0, len(a.Revision.Checkpoints))
		for id := range a.Revision.Checkpoints {
			nodes = append(nodes, id)
		}
		slices.Sort(nodes)
		for _, id := range nodes {
			if a.Revision.Config.Workflow.Nodes[id].Kind != "qa" || !a.Revision.Config.Workflow.Descendants(id)[a.Attempt.Node] {
				continue
			}
			for _, check := range a.Revision.Checkpoints[id].Result.Checks {
				body += "- " + id + " / " + check.Name + ": passed; evidence `" + check.EvidenceDigest + "`\n"
			}
		}
		body += "\nWorkflow run: `" + a.Run.ID + "`; revision: `" + a.Revision.ID + "`.\n"
		return body + "\n" + mark
	})
	if err != nil {
		return "", nil, err
	}
	if err = freeze(filepath.Join(dir, "published.json"), struct {
		Intent publicationIntent
		URL    string
		Number int
	}{intent, chosen.URL, chosen.Number}); err != nil {
		return "", nil, err
	}
	return chosen.URL, modules, nil
}

// pushBranch creates the output branch only when it is absent, then re-reads
// it: command exit alone would not verify the exact head a PR will expose.
func (b *Broker) pushBranch(ctx context.Context, objects string, t lockedTarget, commit string) error {
	head, err := b.branch(ctx, t.Target.Repository, t.Branch)
	if err != nil {
		return err
	}
	if head == "" {
		if _, err = git(ctx, objects, "push", "--porcelain", "--force-with-lease=refs/heads/"+t.Branch+":", b.remote(t.Target.Repository), commit+":refs/heads/"+t.Branch); err != nil {
			return err
		}
	} else if head != commit {
		return errors.New("output branch changed outside this publication; refusing to overwrite")
	}
	head, err = b.branch(ctx, t.Target.Repository, t.Branch)
	if err != nil || head != commit {
		return errors.New("remote output branch does not match the approved commit")
	}
	return nil
}

// reconcilePR adopts the one PR carrying this publication's marker, or opens a
// draft. A lost create acknowledgement reconciles to that PR on replay.
func (b *Broker) reconcilePR(ctx context.Context, t lockedTarget, commit, mark, title string, body func(string) string) (pull, error) {
	var pulls []pull
	if err := b.API.Call(ctx, "GET", pullList(t.Target, t.Branch), nil, &pulls); err != nil {
		return pull{}, err
	}
	if len(pulls) > 0 {
		if len(pulls) != 1 || !verified(pulls[0], t, commit, mark) {
			return pull{}, errors.New("existing PR does not match this approved publication")
		}
		return pulls[0], nil
	}
	if len(title) > 200 {
		title = title[:200]
	}
	if title == "" {
		title = "envctl approved change"
	}
	var chosen pull
	request := map[string]any{"title": title, "head": t.Branch, "base": t.Target.Base, "body": body(mark), "draft": true, "maintainer_can_modify": false}
	if err := b.API.Call(ctx, "POST", "repos/"+t.Target.Repository+"/pulls", request, &chosen); err != nil {
		return pull{}, err
	}
	if !verified(chosen, t, commit, mark) {
		return pull{}, errors.New("created PR does not expose the approved commit")
	}
	return chosen, nil
}
