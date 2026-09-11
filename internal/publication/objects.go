package publication

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// A published commit may reference submodule commits and Git LFS objects that
// exist only in the checkpoint's retained companion. Every such object is made
// available at its own destination before any branch that references it.

type moduleNode struct {
	entry  repository.CompanionRepository
	rel    string // path within the parent repository
	parent *moduleNode
	base   string // gitlink commit before the change, when known
	name   string // resolved GitHub owner/repository, memoized
}

func (n *moduleNode) changed() bool { return n.parent != nil && n.entry.Commit != n.base }

type moduleIntent struct {
	Binding, WorkDigest, Path, Repository, Commit, Branch, Base string
}

type modulePR struct{ Path, URL string }

const lfsTimeout = 10 * time.Minute

func notFound(err error) bool {
	var h HTTPError
	return errors.As(err, &h) && (h.Code == 404 || h.Code == 422)
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:12]
}

// companion loads the retained companion for a changed repository. Results
// without one (captured before companions existed) may only publish commits
// that neither move a gitlink nor use Git LFS.
func (b *Broker) companion(ctx context.Context, objects string, repo workflow.Repository, r workflow.Result, pin, commit string) (*repository.Companion, error) {
	artifact, ok := r.SourceObjects[repo.ID]
	if !ok {
		before, err := gitlinks(ctx, objects, pin)
		if err != nil {
			return nil, err
		}
		after, err := gitlinks(ctx, objects, commit)
		if err != nil {
			return nil, err
		}
		for path, pinned := range after {
			if before[path] != pinned {
				return nil, errors.New("changed submodule " + path + " has no retained submodule/LFS companion to publish")
			}
		}
		if declared, err := lfsDeclared(ctx, objects, commit); err != nil || declared {
			return nil, errors.Join(err, errors.New("Git LFS source has no retained submodule/LFS companion to publish"))
		}
		return nil, nil
	}
	raw, err := b.Store.Artifact(artifact.Digest)
	if err != nil || int64(len(raw)) != artifact.Size || artifact.MediaType != repository.ObjectsMediaType {
		return nil, errors.New("publication submodule/LFS companion is missing or corrupt")
	}
	c, err := repository.ReadCompanion(raw, commit)
	if err != nil {
		return nil, fmt.Errorf("publication submodule/LFS companion rejected: %w", err)
	}
	return c, nil
}

// moduleTree fetches every retained submodule bundle and verifies the
// companion against the committed gitlinks, parents before children. The
// returned order is children first, so publication never exposes a parent
// that references an unpublished child.
func moduleTree(ctx context.Context, objects, scratch string, c *repository.Companion, pin string) ([]*moduleNode, error) {
	entries := map[string]repository.CompanionRepository{}
	root := &moduleNode{base: pin}
	for _, e := range c.Repositories {
		if e.Path == "." {
			root.entry = e
			continue
		}
		entries[e.Path] = e
		body, _ := c.Payload(e.Bundle)
		if err := fetchBundle(ctx, objects, scratch, body, e.Commit); err != nil {
			return nil, errors.New("submodule " + e.Path + ": retained bundle does not provide its commit")
		}
	}
	visited := map[string]bool{}
	var order []*moduleNode
	var walk func(*moduleNode) error
	walk = func(n *moduleNode) error {
		links, err := gitlinks(ctx, objects, n.entry.Commit)
		if err != nil {
			return err
		}
		rels := make([]string, 0, len(links))
		for rel := range links {
			rels = append(rels, rel)
		}
		slices.Sort(rels)
		for _, rel := range rels {
			path := rel
			if n.parent != nil {
				path = n.entry.Path + "/" + rel
			}
			e, ok := entries[path]
			if !ok || e.Commit != links[rel] || visited[path] {
				return errors.New("submodule/LFS companion does not match the committed gitlink at " + path)
			}
			visited[path] = true
			child := &moduleNode{entry: e, rel: rel, parent: n}
			if n.base != "" {
				if child.base, err = gitlinkAt(ctx, objects, n.base, rel); err != nil {
					return err
				}
			}
			if err = walk(child); err != nil {
				return err
			}
			order = append(order, child)
		}
		return nil
	}
	if err := walk(root); err != nil {
		return nil, err
	}
	if len(visited) != len(entries) {
		return nil, errors.New("submodule/LFS companion contains submodules the commit does not reference")
	}
	return append(order, root), nil
}

func fetchBundle(ctx context.Context, objects, scratch string, body []byte, commit string) error {
	file, err := os.CreateTemp(scratch, ".bundle-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err = file.Write(body); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if _, err = git(ctx, objects, "bundle", "verify", file.Name()); err != nil {
		return err
	}
	_, err = git(ctx, objects, "fetch", "--quiet", "--no-tags", file.Name(), commit)
	return err
}

// gitlinks lists a commit's submodule entries without buffering its full tree.
func gitlinks(ctx context.Context, dir, commit string) (map[string]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := gitCommand(ctx, dir, "ls-tree", "-r", "-z", "--full-tree", commit)
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		return nil, errors.New("broker Git operation failed to run")
	}
	links := map[string]string{}
	reader := bufio.NewReader(out)
	var parseErr error
	for {
		record, err := reader.ReadString(0)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			parseErr = err
			break
		}
		meta, name, ok := strings.Cut(strings.TrimSuffix(record, "\x00"), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 3 {
			parseErr = errors.New("invalid Git tree listing")
			break
		}
		if fields[0] == "160000" {
			if len(links) >= 256 {
				parseErr = errors.New("submodule inventory exceeds 256 repositories")
				break
			}
			links[name] = fields[2]
		}
	}
	if parseErr != nil {
		_ = cmd.Process.Kill()
	}
	_, _ = io.Copy(io.Discard, reader)
	if err = cmd.Wait(); err != nil && parseErr == nil {
		parseErr = errors.New("broker Git tree listing failed; the commit is unavailable")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return links, parseErr
}

// gitlinkAt returns the gitlink at one path of a commit, or "" when the commit
// is not retained or the path is not a submodule there.
func gitlinkAt(ctx context.Context, dir, commit, rel string) (string, error) {
	if _, code, err := gitStatus(ctx, dir, 2*time.Minute, "cat-file", "-e", commit+"^{commit}"); err != nil || code != 0 {
		return "", err
	}
	out, err := git(ctx, dir, "--literal-pathspecs", "ls-tree", "-z", commit, "--", rel)
	if err != nil {
		return "", err
	}
	for _, record := range strings.Split(out, "\x00") {
		meta, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(meta)
		if ok && name == rel && len(fields) == 3 && fields[0] == "160000" {
			return fields[2], nil
		}
	}
	return "", nil
}

// moduleURL reads a submodule's URL from its parent commit's .gitmodules.
func moduleURL(ctx context.Context, dir, parentCommit, rel string) (string, error) {
	blob := parentCommit + ":.gitmodules"
	out, code, err := gitStatus(ctx, dir, 2*time.Minute, "config", "--blob", blob, "-z", "--get-regexp", `^submodule\..*\.path$`)
	if err != nil {
		return "", err
	}
	missing := errors.New("submodule " + rel + " has no URL in its parent's .gitmodules")
	if code != 0 {
		return "", missing
	}
	for _, record := range strings.Split(out, "\x00") {
		key, value, ok := strings.Cut(record, "\n")
		if !ok || value != rel || !strings.HasPrefix(key, "submodule.") || !strings.HasSuffix(key, ".path") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(key, "submodule."), ".path")
		url, code, err := gitStatus(ctx, dir, 2*time.Minute, "config", "--blob", blob, "-z", "--get", "submodule."+name+".url")
		if err != nil || code != 0 {
			return "", errors.Join(err, missing)
		}
		return strings.TrimSuffix(url, "\x00"), nil
	}
	return "", missing
}

// moduleRepository resolves a .gitmodules URL to a GitHub repository. Relative
// URLs follow Git: they resolve against the parent repository's own URL.
func moduleRepository(parent, raw string) (string, error) {
	if !strings.HasPrefix(raw, "./") && !strings.HasPrefix(raw, "../") {
		return githubName(raw)
	}
	parts := strings.Split(parent, "/")
	rest := raw
	for {
		if strings.HasPrefix(rest, "./") {
			rest = rest[2:]
		} else if strings.HasPrefix(rest, "../") && len(parts) > 0 {
			parts, rest = parts[:len(parts)-1], rest[3:]
		} else {
			break
		}
	}
	name := strings.TrimSuffix(strings.Trim(strings.Join(append(parts, rest), "/"), "/"), ".git")
	if !repositoryName.MatchString(name) {
		return "", errors.New("relative submodule URL " + raw + " does not resolve to a GitHub repository")
	}
	return name, nil
}

func (b *Broker) moduleName(ctx context.Context, objects string, locked lockedTarget, n *moduleNode) (string, error) {
	if n.parent == nil {
		return locked.Target.Repository, nil
	}
	if n.name != "" {
		return n.name, nil
	}
	parent, err := b.moduleName(ctx, objects, locked, n.parent)
	if err != nil {
		return "", err
	}
	raw, err := moduleURL(ctx, objects, n.parent.entry.Commit, n.rel)
	if err != nil {
		return "", err
	}
	if n.name, err = moduleRepository(parent, raw); err != nil {
		return "", errors.New("submodule " + n.entry.Path + ": " + err.Error())
	}
	return n.name, nil
}

// verifyObjects checks everything publication will depend on before the
// first external effect: no custom LFS endpoint, a committed LFS inventory
// equal to the retained one, and git-lfs when any object must be uploaded.
func verifyObjects(ctx context.Context, objects string, nodes []*moduleNode) error {
	needLFS := false
	for _, n := range nodes {
		if n.parent != nil && !n.changed() {
			continue
		}
		override, err := lfsOverride(ctx, objects, n.entry.Commit)
		if err != nil {
			return err
		}
		if override {
			return errors.New(label(n) + " configures a custom Git LFS endpoint in .lfsconfig; the publication broker only uploads to the GitHub destination")
		}
		declared, err := lfsDeclared(ctx, objects, n.entry.Commit)
		if err != nil {
			return err
		}
		if len(n.entry.LFS) > 0 || declared {
			needLFS = true
		}
		if declared {
			if err = lfsAvailable(ctx, objects); err != nil {
				return err
			}
			inventory, err := lfsInventory(ctx, objects, n.entry.Commit)
			if err != nil {
				return err
			}
			if !slices.Equal(inventory, n.entry.LFS) {
				return errors.New(label(n) + ": committed Git LFS pointers differ from the retained LFS objects")
			}
		}
	}
	if needLFS {
		return lfsAvailable(ctx, objects)
	}
	return nil
}

func label(n *moduleNode) string {
	if n.parent == nil {
		return "repository"
	}
	return "submodule " + n.entry.Path
}

func lfsAvailable(ctx context.Context, dir string) error {
	if _, code, err := gitStatus(ctx, dir, 30*time.Second, "lfs", "version"); err != nil || code != 0 {
		return errors.New("publication includes Git LFS objects, but git-lfs is not installed for the coordinator's git; install git-lfs (https://git-lfs.com) on this host and retry")
	}
	return nil
}

// lfsDeclared reports whether a commit's attributes route any path through
// Git LFS, or it carries LFS configuration.
func lfsDeclared(ctx context.Context, dir, commit string) (bool, error) {
	_, code, err := gitStatus(ctx, dir, 2*time.Minute, "grep", "-l", "-z", "-F", "-e", "filter=lfs", commit, "--", ".gitattributes", ":(glob)**/.gitattributes")
	if err != nil || code > 1 {
		return false, errors.Join(err, errors.New("broker could not inspect Git LFS attributes"))
	}
	if code == 0 {
		return true, nil
	}
	_, code, err = gitStatus(ctx, dir, 2*time.Minute, "cat-file", "-e", commit+":.lfsconfig")
	return err == nil && code == 0, err
}

func lfsOverride(ctx context.Context, dir, commit string) (bool, error) {
	if _, code, err := gitStatus(ctx, dir, 2*time.Minute, "cat-file", "-e", commit+":.lfsconfig"); err != nil || code != 0 {
		return false, err
	}
	_, code, err := gitStatus(ctx, dir, 2*time.Minute, "config", "--blob", commit+":.lfsconfig", "--get-regexp", `^(lfs\.url|lfs\.pushurl|remote\..*\.lfsurl|remote\..*\.lfspushurl)$`)
	if err != nil || code > 1 {
		return false, errors.Join(err, errors.New("broker could not read .lfsconfig"))
	}
	return code == 0, nil
}

// lfsInventory lists a commit's LFS pointers in the order objects.py records.
func lfsInventory(ctx context.Context, dir, commit string) ([]repository.CompanionLFS, error) {
	out, err := git(ctx, dir, "lfs", "ls-files", "--include=", "--exclude=", "--json", commit)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Files []struct {
			Name    string `json:"name"`
			OID     string `json:"oid"`
			OIDType string `json:"oid_type"`
			Size    int64  `json:"size"`
		} `json:"files"`
	}
	if err = json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, errors.New("invalid Git LFS inventory")
	}
	inventory := []repository.CompanionLFS{}
	for _, f := range doc.Files {
		if f.OIDType != "sha256" {
			return nil, errors.New("unsupported Git LFS object")
		}
		inventory = append(inventory, repository.CompanionLFS{Path: f.Name, OID: f.OID, Size: f.Size})
	}
	slices.SortFunc(inventory, func(x, y repository.CompanionLFS) int { return strings.Compare(x.Path, y.Path) })
	return inventory, nil
}

// pushLFS uploads retained LFS objects from the broker's object store. The
// LFS batch API skips objects the destination already has, so replay after a
// crash is safe.
func (b *Broker) pushLFS(ctx context.Context, objects, name string, c *repository.Companion, files []repository.CompanionLFS) error {
	if len(files) == 0 {
		return nil
	}
	if err := lfsAvailable(ctx, objects); err != nil {
		return err
	}
	oids := []string{}
	for _, f := range files {
		if slices.Contains(oids, f.OID) {
			continue
		}
		body, ok := c.Payload(f.OID)
		if !ok {
			return errors.New("retained Git LFS object is missing")
		}
		if err := stageLFS(objects, f.OID, body); err != nil {
			return err
		}
		oids = append(oids, f.OID)
	}
	slices.Sort(oids)
	args := append([]string{"lfs", "push", "--object-id", b.remote(name)}, oids...)
	if _, err := gitTimeout(ctx, objects, lfsTimeout, args...); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Git LFS upload to github.com/" + name + " failed; verify the coordinator's gh connection can push there and Git LFS is enabled")
	}
	return nil
}

func stageLFS(objects, oid string, body []byte) error {
	dir := filepath.Join(objects, "lfs", "objects", oid[:2], oid[2:4])
	target := filepath.Join(dir, oid)
	if existing, err := os.ReadFile(target); err == nil {
		sum := sha256.Sum256(existing)
		if hex.EncodeToString(sum[:]) == oid {
			return nil
		}
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".object-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(body); err != nil {
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
	return os.Rename(f.Name(), target)
}

// publishModules publishes changed submodules, children first. Unchanged
// gitlinks were already referenced by the published base and need nothing.
func (b *Broker) publishModules(ctx context.Context, a engine.Assignment, repo workflow.Repository, r workflow.Result, dir, objects string, locked lockedTarget, c *repository.Companion, nodes []*moduleNode) ([]modulePR, error) {
	var prs []modulePR
	for _, n := range nodes {
		if !n.changed() {
			continue
		}
		name, err := b.moduleName(ctx, objects, locked, n)
		if err != nil {
			return nil, err
		}
		var children []modulePR
		for _, pr := range prs {
			if strings.HasPrefix(pr.Path, n.entry.Path+"/") {
				children = append(children, pr)
			}
		}
		url, err := b.publishModule(ctx, a, repo, r, dir, objects, locked, c, n, name, children)
		if err != nil {
			return nil, err
		}
		if url != "" {
			prs = append(prs, modulePR{Path: n.entry.Path, URL: url})
		}
	}
	return prs, nil
}

func (b *Broker) publishModule(ctx context.Context, a engine.Assignment, repo workflow.Repository, r workflow.Result, dir, objects string, locked lockedTarget, c *repository.Companion, n *moduleNode, name string, children []modulePR) (string, error) {
	fail := func(err error) (string, error) { return "", fmt.Errorf("submodule %s: %w", n.entry.Path, err) }
	mdir := filepath.Join(dir, "modules", shortHash(n.entry.Path))
	var intent moduleIntent
	if err := load(filepath.Join(mdir, "intent.json"), &intent); errors.Is(err, os.ErrNotExist) {
		// A commit the destination already has (upstream history the worker
		// only checked out) needs no publication.
		if _, err = b.commit(ctx, name, n.entry.Commit); err == nil {
			return "", nil
		} else if !notFound(err) {
			return fail(err)
		}
		var info struct {
			Archived      bool   `json:"archived"`
			Disabled      bool   `json:"disabled"`
			DefaultBranch string `json:"default_branch"`
			Permissions   struct {
				Push bool `json:"push"`
			} `json:"permissions"`
		}
		if err = b.API.Call(ctx, "GET", "repos/"+name, nil, &info); err != nil {
			return fail(err)
		}
		if info.Archived || info.Disabled || !info.Permissions.Push {
			return fail(fmt.Errorf("commit %s is not at github.com/%s and the coordinator's gh connection cannot push there; publish it or grant push access, then retry", n.entry.Commit, name))
		}
		branch := locked.Branch + "-" + shortHash(n.entry.Path)
		if !validRef(info.DefaultBranch) || !validRef(branch) {
			return fail(errors.New("invalid submodule base or output branch"))
		}
		intent = moduleIntent{Binding: locked.Binding, WorkDigest: r.WorkDigest(), Path: n.entry.Path, Repository: name, Commit: n.entry.Commit, Branch: branch, Base: info.DefaultBranch}
		if err = freeze(filepath.Join(mdir, "intent.json"), intent); err != nil {
			return fail(err)
		}
	} else if err != nil {
		return fail(err)
	}
	if intent.Binding != locked.Binding || intent.WorkDigest != r.WorkDigest() || intent.Path != n.entry.Path || intent.Commit != n.entry.Commit || intent.Repository != name {
		return fail(errors.New("publication receipt conflicts with this approved result"))
	}
	if err := b.pushLFS(ctx, objects, name, c, n.entry.LFS); err != nil {
		return fail(err)
	}
	t := lockedTarget{Binding: intent.Binding, Target: workflow.PublicationTarget{Provider: "github", Repository: name, Base: intent.Base}, Branch: intent.Branch}
	if err := b.pushBranch(ctx, objects, t, intent.Commit); err != nil {
		return fail(err)
	}
	title := strings.TrimSpace(fmt.Sprintf("%s (submodule %s)", strings.TrimSpace(a.Run.Name), n.entry.Path))
	chosen, err := b.reconcilePR(ctx, t, intent.Commit, marker(a, repo.ID+"/"+n.entry.Path, r), title, func(mark string) string {
		body := fmt.Sprintf("Submodule `%s` commit `%s`, required by the envctl change on `%s` branch `%s`.\n\n**Merge this pull request before the parent change**, which references this commit.\n", n.entry.Path, intent.Commit, locked.Target.Repository, locked.Branch)
		body += modulesSection(children)
		return body + "\nWorkflow run: `" + a.Run.ID + "`; revision: `" + a.Revision.ID + "`.\n\n" + mark
	})
	if err != nil {
		return fail(err)
	}
	if err = freeze(filepath.Join(mdir, "published.json"), struct {
		Intent moduleIntent
		URL    string
		Number int
	}{intent, chosen.URL, chosen.Number}); err != nil {
		return fail(err)
	}
	return chosen.URL, nil
}

func modulesSection(prs []modulePR) string {
	if len(prs) == 0 {
		return ""
	}
	var body bytes.Buffer
	body.WriteString("\nSubmodule changes (merge these first):\n")
	for _, pr := range prs {
		fmt.Fprintf(&body, "- `%s`: %s\n", pr.Path, pr.URL)
	}
	return body.String()
}
