package workflow

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

var ErrConflict = errors.New("stale run version")
var shaRE = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)
var digestRE = regexp.MustCompile(`^[a-f0-9]{64}$`)

func ID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return prefix + "_" + hex.EncodeToString(b)
}
func Digest(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

type Run struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	TaskRef         string     `json:"task_ref,omitempty"`
	Owner           string     `json:"owner"`
	Priority        int        `json:"priority"`
	Version         int64      `json:"version"`
	CurrentRevision string     `json:"current_revision"`
	Revisions       []Revision `json:"revisions"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}
type Revision struct {
	ID                     string                   `json:"id"`
	Parent                 string                   `json:"parent,omitempty"`
	FromCheckpoint         string                   `json:"from_checkpoint,omitempty"`
	Objective              string                   `json:"objective"`
	Config                 Config                   `json:"config"`
	State                  string                   `json:"state"`
	Runtime                RuntimeState             `json:"runtime"`
	ChildRuntimes          map[string]*ChildRuntime `json:"child_runtimes,omitempty"`
	Readiness              []Probe                  `json:"readiness"`
	Attempts               []Attempt                `json:"attempts"`
	Checkpoints            map[string]Checkpoint    `json:"checkpoints"`
	Messages               []Message                `json:"messages"`
	CreatedAt              time.Time                `json:"created_at"`
	Recovery               *Recovery                `json:"recovery,omitempty"`
	SourcePins             map[string]string        `json:"source_pins,omitempty"`
	ReadinessCheckedAt     time.Time                `json:"readiness_checked_at,omitempty"`
	DrainNodes             []string                 `json:"drain_nodes,omitempty"`
	DiscoveredRequirements []Requirement            `json:"discovered_requirements,omitempty"`
}

// Recovery is visible, retryable work. Known missing requirements remain in
// planning; they cannot be waived by an agent's completion claim.
type Recovery struct {
	Phase          string    `json:"phase"`
	Detail         string    `json:"detail"`
	EvidenceDigest string    `json:"evidence_digest"`
	Failures       int       `json:"failures"`
	RetryAt        time.Time `json:"retry_at"`
}
type RuntimeState struct {
	ID            string    `json:"id"`
	Provider      string    `json:"provider"`
	Location      string    `json:"location"`
	Ready         bool      `json:"ready"`
	State         string    `json:"state"`
	PreviewURL    string    `json:"preview_url,omitempty"`
	DaemonID      string    `json:"daemon_id,omitempty"`
	ImageDigest   string    `json:"image_digest,omitempty"`
	ComposeDigest string    `json:"compose_digest,omitempty"`
	Services      []Service `json:"services,omitempty"`
}
type Service struct {
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url,omitempty"`
}
type Probe struct {
	Capability     string    `json:"capability"`
	Binding        string    `json:"binding"`
	ConfigDigest   string    `json:"config_digest"`
	RuntimeID      string    `json:"runtime_id"`
	Passed         bool      `json:"passed"`
	Detail         string    `json:"detail"`
	EvidenceDigest string    `json:"evidence_digest"`
	CheckedAt      time.Time `json:"checked_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}
type Message struct {
	ID        string    `json:"id"`
	Node      string    `json:"node,omitempty"`
	Recipient string    `json:"recipient"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}
type Attempt struct {
	ID        string            `json:"id"`
	Node      string            `json:"node"`
	State     string            `json:"state"`
	Number    int               `json:"number"`
	Inputs    map[string]string `json:"inputs"`
	StartedAt time.Time         `json:"started_at"`
	UpdatedAt time.Time         `json:"updated_at"`
	Error     string            `json:"error,omitempty"`
	Session   string            `json:"session,omitempty"`
	Result    *Result           `json:"result,omitempty"`
	Approval  *Approval         `json:"approval,omitempty"`
	RetryAt   time.Time         `json:"retry_at,omitempty"`
}
type Artifact struct {
	Name      string `json:"name"`
	Digest    string `json:"digest"`
	Size      int64  `json:"size"`
	MediaType string `json:"media_type"`
}
type CheckResult struct {
	Name           string `json:"name"`
	Passed         bool   `json:"passed"`
	ExitCode       int    `json:"exit_code"`
	EvidenceDigest string `json:"evidence_digest"`
	CommitsDigest  string `json:"commits_digest"`
}
type Review struct {
	Accepted       bool   `json:"accepted"`
	Summary        string `json:"summary"`
	EvidenceDigest string `json:"evidence_digest"`
	ResultDigest   string `json:"result_digest"`
}
type Approval struct {
	Actor        string    `json:"actor"`
	ResultDigest string    `json:"result_digest"`
	At           time.Time `json:"at"`
}
type Result struct {
	Requirements  []Requirement                `json:"requirements,omitempty"`
	Summary       string                       `json:"summary"`
	Artifacts     []Artifact                   `json:"artifacts"`
	Sources       map[string]Artifact          `json:"sources,omitempty"`        // repository ID -> restorable Git bundle
	SourceObjects map[string]Artifact          `json:"source_objects,omitempty"` // companion submodule bundles and LFS objects
	MergeParents  map[string]map[string]string `json:"merge_parents,omitempty"`  // repository -> direct input node -> required ancestor SHA
	Data          map[string]any               `json:"data,omitempty"`
	Commits       map[string]string            `json:"commits"`
	Checks        []CheckResult                `json:"checks"`
	Review        Review                       `json:"review"`
	PRs           map[string]string            `json:"prs,omitempty"`
	DatasetDigest string                       `json:"dataset_digest,omitempty"`
	Datasets      map[string]DatasetSnapshot   `json:"datasets,omitempty"`
}

// WorkDigest excludes the reviewer and publication receipts: both refer to this
// exact work product. A changed artifact or commit invalidates review/approval.
func (r Result) WorkDigest() string { r.Review = Review{}; r.PRs = nil; return Digest(r) }

type Checkpoint struct {
	ID             string            `json:"id"`
	Revision       string            `json:"revision"`
	Node           string            `json:"node"`
	Attempt        string            `json:"attempt"`
	Inputs         map[string]string `json:"inputs"`
	ConfigDigest   string            `json:"config_digest"`
	Result         Result            `json:"result"`
	Approval       *Approval         `json:"approval,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
	HistoricalOnly bool              `json:"historical_only,omitempty"`
}

func NewRun(name, objective, owner string, c Config, now time.Time) (*Run, error) {
	if strings.TrimSpace(objective) == "" {
		return nil, errors.New("task objective is required")
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	rev := Revision{ID: ID("rev"), Objective: objective, Config: Clone(c), State: "queued", Checkpoints: map[string]Checkpoint{}, CreatedAt: now}
	return &Run{ID: ID("run"), Name: name, Description: objective, Owner: owner, Version: 1, CurrentRevision: rev.ID, Revisions: []Revision{rev}, CreatedAt: now, UpdatedAt: now}, nil
}
func (r *Run) Current() *Revision { return r.Revision(r.CurrentRevision) }
func (r *Run) Revision(id string) *Revision {
	for i := range r.Revisions {
		if r.Revisions[i].ID == id {
			return &r.Revisions[i]
		}
	}
	return nil
}
func (r *Revision) Attempt(id string) *Attempt {
	for i := range r.Attempts {
		if r.Attempts[i].ID == id {
			return &r.Attempts[i]
		}
	}
	return nil
}
func (r *Revision) Active(node string) bool {
	for _, a := range r.Attempts {
		if a.Node == node && slices.Contains([]string{"preparing", "running", "verifying", "awaiting-approval"}, a.State) {
			return true
		}
	}
	return false
}
func (r *Revision) Requirements() []string {
	seen := map[string]bool{"harness.worker": true, "harness.supervisor": true, "runtime.compose": true, "repositories.readwrite": true}
	for _, requirement := range r.DiscoveredRequirements {
		seen[requirement.Capability] = true
	}
	if len(r.Config.Data.Datasets) > 0 {
		seen["dataset.restore"] = true
	}
	for _, p := range r.Config.Plugins {
		for _, c := range append(append([]string(nil), p.Provides...), p.Requires...) {
			seen[c] = true
		}
	}
	for _, n := range r.Config.Workflow.Nodes {
		if n.Kind == "qa" || len(n.Checks) > 0 {
			seen["workflow.checks"] = true
		}
		if n.Kind == "change" {
			seen["publication.pr"] = true
		}
		for _, c := range n.Requires {
			seen[c] = true
		}
	}
	out := []string{}
	for c := range seen {
		out = append(out, c)
	}
	slices.Sort(out)
	return out
}
func (r *Revision) ReadinessProblems(now time.Time, capabilities []string) []string {
	var out []string
	cfg := Digest(r.Config)
	for _, c := range capabilities {
		var p *Probe
		for i := range r.Readiness {
			if r.Readiness[i].Capability == c {
				p = &r.Readiness[i]
			}
		}
		switch {
		case p == nil:
			out = append(out, c+": not probed")
		case !p.Passed:
			out = append(out, c+": "+p.Detail)
		case p.ConfigDigest != cfg || p.RuntimeID != r.Runtime.ID:
			out = append(out, c+": binding changed")
		case p.Binding == "" || !digestRE.MatchString(p.EvidenceDigest) || p.CheckedAt.IsZero() || p.CheckedAt.After(now):
			out = append(out, c+": missing or invalid evidence")
		case p.ExpiresAt.IsZero() || !p.ExpiresAt.After(now):
			out = append(out, c+": evidence expired")
		}
	}
	return out
}
func (r *Revision) SetProbe(p Probe) {
	for i := range r.Readiness {
		if r.Readiness[i].Capability == p.Capability {
			r.Readiness[i] = p
			return
		}
	}
	r.Readiness = append(r.Readiness, p)
}
func (r *Revision) ReadyNodes(now time.Time) []string {
	if (r.State != "active" && r.State != "draining") || !r.Runtime.Ready {
		return nil
	}
	out := []string{}
	for id, n := range r.Config.Workflow.Nodes {
		if r.State == "draining" && !slices.Contains(r.DrainNodes, id) {
			continue
		}
		if _, ok := r.Checkpoints[id]; ok || r.Active(id) {
			continue
		}
		satisfied := true
		for _, dep := range n.Needs {
			if cp, ok := r.Checkpoints[dep]; !ok || cp.HistoricalOnly {
				satisfied = false
				break
			}
		}
		if !satisfied {
			continue
		}
		if n.Kind != "task" && n.Kind != "plan" && len(r.ReadinessProblems(now, r.Requirements())) > 0 {
			continue
		}
		attempts := 0
		retryReady := true
		for _, a := range r.Attempts {
			if a.Node == id {
				attempts++
				if a.State == "failed" && a.RetryAt.After(now) {
					retryReady = false
				}
			}
		}
		if attempts >= r.Config.NodeLimits(id).MaxAttempts || !retryReady {
			continue
		}
		out = append(out, id)
	}
	slices.SortFunc(out, func(a, b string) int {
		pa, pb := r.Config.Workflow.Nodes[a].Priority, r.Config.Workflow.Nodes[b].Priority
		if pa != pb {
			if pa > pb {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	})
	return out
}
func (r *Revision) Begin(node string, now time.Time) (*Attempt, error) {
	if !slices.Contains(r.ReadyNodes(now), node) {
		return nil, fmt.Errorf("node %s is not runnable", node)
	}
	active := 0
	number := 1
	for _, a := range r.Attempts {
		if r.Active(a.Node) && slices.Contains([]string{"preparing", "running", "verifying", "awaiting-approval"}, a.State) {
			active++
		}
		if a.Node == node {
			number++
		}
	}
	if active >= r.Config.Limits.Parallel {
		return nil, errors.New("worker capacity reached")
	}
	inputs := map[string]string{}
	for _, dep := range r.Config.Workflow.Nodes[node].Needs {
		inputs[dep] = r.Checkpoints[dep].ID
	}
	r.Attempts = append(r.Attempts, Attempt{ID: ID("attempt"), Node: node, Number: number, State: "running", Inputs: inputs, StartedAt: now, UpdatedAt: now})
	return &r.Attempts[len(r.Attempts)-1], nil
}
func (r *Revision) Propose(attempt string, result Result, now time.Time) error {
	if r.State != "active" && r.State != "draining" {
		return errors.New("revision is not executing")
	}
	a := r.Attempt(attempt)
	if a == nil || a.State != "running" {
		return errors.New("attempt is not running")
	}
	if err := r.validateResultEvidence(a.Node, result, now, r.State != "draining"); err != nil {
		return err
	}
	if err := r.RecordPlanRequirements(a.Node, result.Requirements); err != nil {
		return err
	}
	a.Result = &result
	a.UpdatedAt = now
	a.State = "verifying"
	if r.Config.Workflow.Nodes[a.Node].Gate == "human" {
		a.State = "awaiting-approval"
	}
	return nil
}
func (r *Revision) validateResult(node string, result Result, now time.Time) error {
	return r.validateResultEvidence(node, result, now, true)
}

// Historical work still requires complete outputs, checks and supervisor
// review. It cannot authorize downstream execution, so a superseded Plan does
// not need to resolve capabilities that only its replacement will execute.
func (r *Revision) validateResultEvidence(node string, result Result, now time.Time, admit bool) error {
	n, ok := r.Config.Workflow.Nodes[node]
	if !ok {
		return errors.New("unknown node")
	}
	parents, err := r.MergeInputs(node)
	if err != nil {
		return err
	}
	if Digest(parents) != Digest(result.MergeParents) {
		return errors.New("merge evidence must name every exact input commit")
	}
	for repo := range parents {
		if !shaRE.MatchString(result.Commits[repo]) || result.Sources[repo].MediaType != "application/x-git-bundle" {
			return errors.New("merge output requires an immutable commit and retained source bundle")
		}
	}
	if strings.TrimSpace(result.Summary) == "" {
		return errors.New("result summary is required")
	}
	if n.Kind != "plan" && len(result.Requirements) > 0 {
		return errors.New("only Plan can declare workflow requirements")
	}
	if err := ValidateRequirements(r.Config.Workflow, result.Requirements); err != nil {
		return err
	}
	artifacts := map[string]bool{}
	for _, a := range result.Artifacts {
		if !identifier.MatchString(a.Name) || artifacts[a.Name] || !digestRE.MatchString(a.Digest) || a.Size < 0 {
			return errors.New("invalid or duplicate artifact")
		}
		artifacts[a.Name] = true
	}
	for _, name := range n.Outputs {
		if !artifacts[name] {
			return fmt.Errorf("required artifact %s missing", name)
		}
	}
	if r.ChildRuntimes[node] != nil && len(result.Sources) != len(r.Config.Repositories) {
		return errors.New("child checkpoint must retain every repository before its VM can be released")
	}
	if len(result.Sources) > 0 {
		if len(result.Sources) != len(r.Config.Repositories) {
			return errors.New("source checkpoint must cover every repository")
		}
		for _, repo := range r.Config.Repositories {
			artifact, ok := result.Sources[repo.ID]
			if !ok || !shaRE.MatchString(result.Commits[repo.ID]) || !digestRE.MatchString(artifact.Digest) || artifact.Size <= 0 || artifact.MediaType != "application/x-git-bundle" {
				return errors.New("invalid repository source checkpoint")
			}
		}
	}
	if len(result.SourceObjects) > 0 {
		if len(result.SourceObjects) != len(r.Config.Repositories) || len(result.Sources) != len(r.Config.Repositories) {
			return errors.New("source object companions must cover every repository bundle")
		}
		for _, repo := range r.Config.Repositories {
			artifact, ok := result.SourceObjects[repo.ID]
			if !ok || !digestRE.MatchString(artifact.Digest) || artifact.Size <= 0 || artifact.MediaType != "application/vnd.envctl.git-objects+tar" {
				return errors.New("invalid source object companion")
			}
		}
	}
	if !result.Review.Accepted || strings.TrimSpace(result.Review.Summary) == "" || !digestRE.MatchString(result.Review.EvidenceDigest) || result.Review.ResultDigest != result.WorkDigest() {
		return errors.New("supervisor must accept this exact work product with evidence")
	}
	if n.Kind != "task" && n.Kind != "plan" && len(r.Config.Data.Datasets) > 0 {
		if len(result.Datasets) != len(r.Config.Data.Datasets) || result.DatasetDigest != Digest(result.Datasets) {
			return errors.New("checkpoint must retain the complete dataset manifest")
		}
		for _, dataset := range r.Config.Data.Datasets {
			snapshot, ok := result.Datasets[dataset.ID]
			if !ok || snapshot.Adapter != dataset.Adapter || snapshot.BindingDigest != Digest(dataset) || snapshot.Format != dataset.SnapshotFormat() || snapshot.ToolVersion == "" || !digestRE.MatchString(snapshot.Source.Digest) || snapshot.Source.Size <= 0 || snapshot.Source.MediaType != dataset.SnapshotMedia() || !digestRE.MatchString(snapshot.Evidence.Digest) || snapshot.Evidence.Size <= 0 || snapshot.Evidence.MediaType != "application/json" {
				return fmt.Errorf("dataset %s has no valid restoration evidence", dataset.ID)
			}
		}
	}
	if len(n.OutputSchema) > 0 {
		compiler := jsonschema.NewCompiler()
		if err := compiler.AddResource("urn:envctl:output", n.OutputSchema); err != nil {
			return fmt.Errorf("output schema: %w", err)
		}
		schema, err := compiler.Compile("urn:envctl:output")
		if err != nil {
			return err
		}
		if err = schema.Validate(result.Data); err != nil {
			return fmt.Errorf("output contract: %w", err)
		}
	}
	if n.Kind == "plan" && admit {
		requirements := r.Requirements()
		for _, requirement := range result.Requirements {
			if !slices.Contains(requirements, requirement.Capability) {
				requirements = append(requirements, requirement.Capability)
			}
		}
		if problems := r.ReadinessProblems(now, requirements); len(problems) > 0 {
			return fmt.Errorf("Plan readiness incomplete: %s", strings.Join(problems, "; "))
		}
	}
	if slices.Contains([]string{"code", "qa", "change"}, n.Kind) {
		for _, repo := range r.Config.Repositories {
			if !shaRE.MatchString(result.Commits[repo.ID]) {
				return fmt.Errorf("missing immutable commit for %s", repo.ID)
			}
		}
	}
	checks := map[string]bool{}
	for _, c := range result.Checks {
		if checks[c.Name] || c.Name == "" || !c.Passed || c.ExitCode != 0 || !digestRE.MatchString(c.EvidenceDigest) || c.CommitsDigest != Digest(result.Commits) {
			return fmt.Errorf("check %s lacks passing evidence for these commits", c.Name)
		}
		checks[c.Name] = true
	}
	for _, c := range n.Checks {
		if !checks[c.Name] {
			return fmt.Errorf("required check %s missing", c.Name)
		}
	}
	if n.Kind == "change" {
		found := false
		for id, ancestor := range r.Config.Workflow.Nodes {
			if ancestor.Kind != "qa" || !r.Config.Workflow.Descendants(id)[node] {
				continue
			}
			cp, ok := r.Checkpoints[id]
			if !ok {
				return errors.New("change requires accepted QA evidence")
			}
			found = true
			if len(r.Config.Data.Datasets) > 0 && result.DatasetDigest != cp.Result.DatasetDigest {
				return errors.New("approved dataset differs from accepted QA")
			}
			for repo, sha := range cp.Result.Commits {
				if result.Commits[repo] != sha {
					return fmt.Errorf("change commit for %s differs from accepted QA", repo)
				}
			}
		}
		if !found {
			return errors.New("approved change requires a QA ancestor")
		}
	}
	if n.Kind == "qa" && len(checks) == 0 {
		return errors.New("QA requires executed test evidence")
	}
	return nil
}

// ValidateResult exposes the same deterministic candidate contract to external
// effect brokers without mutating an attempt or accepting a checkpoint.
func (r *Revision) ValidateResult(node string, result Result, now time.Time) error {
	return r.validateResult(node, result, now)
}
func (r *Run) Approve(revision, attempt, actor, workDigest string, now time.Time) error {
	if revision != r.CurrentRevision {
		return errors.New("cannot approve superseded revision")
	}
	rev := r.Current()
	a := rev.Attempt(attempt)
	if a == nil || a.State != "awaiting-approval" || a.Result == nil {
		return errors.New("attempt is not awaiting approval")
	}
	if strings.TrimSpace(actor) == "" || a.Result.WorkDigest() != workDigest {
		return errors.New("approval must identify actor and exact work digest")
	}
	a.Approval = &Approval{Actor: actor, ResultDigest: workDigest, At: now}
	a.State = "verifying"
	a.UpdatedAt = now
	return nil
}
func (r *Revision) Accept(attempt string, now time.Time) (Checkpoint, error) {
	if r.State != "active" && r.State != "draining" {
		return Checkpoint{}, errors.New("revision is not executing")
	}
	a := r.Attempt(attempt)
	if a == nil || a.State != "verifying" || a.Result == nil {
		return Checkpoint{}, errors.New("attempt not verified")
	}
	if r.State == "draining" {
		if err := r.ArchiveDrained(attempt, now); err != nil {
			return Checkpoint{}, err
		}
		return r.Checkpoints[a.Node], nil
	}
	if err := r.validateResult(a.Node, *a.Result, now); err != nil {
		return Checkpoint{}, err
	}
	n := r.Config.Workflow.Nodes[a.Node]
	if n.Gate == "human" && (a.Approval == nil || a.Approval.ResultDigest != a.Result.WorkDigest()) {
		return Checkpoint{}, errors.New("matching approval required")
	}
	for dep, id := range a.Inputs {
		if r.Checkpoints[dep].ID != id || r.Checkpoints[dep].HistoricalOnly {
			return Checkpoint{}, errors.New("checkpoint inputs changed")
		}
	}
	if n.Kind == "change" {
		changed := 0
		for repo := range a.Result.Commits {
			// Read-only dependency repositories retain their approved pin and
			// do not need an artificial change merely to open another PR.
			if a.Result.Commits[repo] == r.SourcePins[repo] {
				continue
			}
			changed++
			if !strings.HasPrefix(a.Result.PRs[repo], "https://") {
				return Checkpoint{}, fmt.Errorf("verified PR URL required for %s", repo)
			}
		}
		if changed == 0 {
			return Checkpoint{}, errors.New("approved change requires a changed repository and PR output")
		}
	}
	cp := Checkpoint{ID: ID("cp"), Revision: r.ID, Node: a.Node, Attempt: a.ID, Inputs: Clone(a.Inputs), ConfigDigest: Digest(r.Config), Result: Clone(*a.Result), Approval: Clone(a.Approval), CreatedAt: now}
	r.Checkpoints[a.Node] = cp
	a.State = "checkpointed"
	a.UpdatedAt = now
	if r.State == "draining" {
		if !r.HasUnfinishedDrain() {
			r.State = "superseded"
		}
	} else if len(r.Checkpoints) == len(r.Config.Workflow.Nodes) {
		r.State = "completed"
	}
	return cp, nil
}
func (r *Revision) HasActive() bool {
	for _, a := range r.Attempts {
		if r.Active(a.Node) {
			return true
		}
	}
	return false
}

// ArchiveDrained seals the old worker's evidence without approval/publication.
// A historical checkpoint is reviewable but can never satisfy a new DAG edge.
func (r *Revision) ArchiveDrained(attempt string, now time.Time) error {
	if r.State != "draining" {
		return errors.New("only superseded work can be archived")
	}
	a := r.Attempt(attempt)
	if a == nil || a.Result == nil || !slices.Contains([]string{"verifying", "awaiting-approval"}, a.State) {
		return errors.New("draining result is not verified")
	}
	if err := r.validateResultEvidence(a.Node, *a.Result, now, false); err != nil {
		return err
	}
	r.Checkpoints[a.Node] = Checkpoint{ID: ID("cp"), Revision: r.ID, Node: a.Node, Attempt: a.ID, Inputs: Clone(a.Inputs), ConfigDigest: Digest(r.Config), Result: Clone(*a.Result), CreatedAt: now, HistoricalOnly: true}
	a.State, a.UpdatedAt = "checkpointed", now
	if !r.HasUnfinishedDrain() {
		r.State = "superseded"
	}
	return nil
}
func (r *Revision) Fail(attempt, reason string, now time.Time) error {
	a := r.Attempt(attempt)
	if a == nil || !slices.Contains([]string{"running", "preparing", "verifying", "awaiting-approval"}, a.State) {
		return errors.New("attempt is not active")
	}
	a.State = "failed"
	a.Error = reason
	a.UpdatedAt = now
	return nil
}

func (r *Revision) HasUnfinishedDrain() bool {
	for _, node := range r.DrainNodes {
		if _, done := r.Checkpoints[node]; !done {
			return true
		}
	}
	return r.HasActive()
}
func (r *Run) Cancel(now time.Time) {
	for i := range r.Revisions {
		rev := &r.Revisions[i]
		if slices.Contains([]string{"queued", "preparing", "active", "draining", "recovering", "needs-attention", "completed"}, rev.State) {
			rev.State = "cancelled"
			for j := range rev.Attempts {
				a := &rev.Attempts[j]
				if slices.Contains([]string{"preparing", "running", "verifying", "awaiting-approval"}, a.State) {
					a.State = "cancelled"
					a.UpdatedAt = now
				}
			}
		}
	}
}

// Rewind invalidates the selected node and descendants. Unaffected checkpoints
// stay immutable and retain their original provenance. The runtime must restore
// from the input checkpoint, never from the superseded worker's live disk.
func (r *Run) Rewind(node, objective string, config *Config, now time.Time) (string, error) {
	old := r.Current()
	if old == nil {
		return "", errors.New("current revision missing")
	}
	if _, ok := old.Config.Workflow.Nodes[node]; !ok {
		return "", errors.New("unknown rewind node")
	}
	c := Clone(old.Config)
	if config != nil {
		c = Clone(*config)
		if err := c.Validate(); err != nil {
			return "", err
		}
	}
	if objective == "" {
		objective = old.Objective
	}
	invalid := old.Config.Workflow.Descendants(node)
	invalid[node] = true
	if objective != old.Objective {
		for id := range old.Config.Workflow.Nodes {
			invalid[id] = true
		}
	}
	// Capability or specification changes always reopen Plan, since the old
	// readiness evidence is tied to the previous runtime/configuration.
	if Digest(c) != Digest(old.Config) {
		plan := old.Config.Workflow.PlanID()
		invalid[plan] = true
		for id := range old.Config.Workflow.Descendants(plan) {
			invalid[id] = true
		}
	}
	pins := map[string]string{}
	for _, repo := range c.Repositories {
		if repo.BaseSHA != "" {
			pins[repo.ID] = repo.BaseSHA
			continue
		}
		for _, prior := range old.Config.Repositories {
			if prior.ID == repo.ID && prior.URL == repo.URL && prior.Ref == repo.Ref {
				if pin := old.SourcePins[repo.ID]; pin != "" {
					pins[repo.ID] = pin
				}
			}
		}
	}
	rev := Revision{ID: ID("rev"), Parent: old.ID, Objective: objective, Config: c, State: "queued", Checkpoints: map[string]Checkpoint{}, SourcePins: pins, CreatedAt: now}
	if objective == old.Objective && Digest(c.Workflow) == Digest(old.Config.Workflow) {
		rev.DiscoveredRequirements = Clone(old.DiscoveredRequirements)
	}
	for id, cp := range old.Checkpoints {
		if next, exists := c.Workflow.Nodes[id]; exists && !cp.HistoricalOnly && !invalid[id] && Digest(next) == Digest(old.Config.Workflow.Nodes[id]) {
			rev.Checkpoints[id] = Clone(cp)
		}
	}
	order, _ := old.Config.Workflow.Order()
	for _, id := range order {
		if cp, ok := rev.Checkpoints[id]; ok {
			rev.FromCheckpoint = cp.ID
		}
	}
	for i := range r.Revisions {
		prev := &r.Revisions[i]
		if prev.State == "queued" || prev.State == "preparing" || prev.State == "needs-attention" || prev.State == "completed" {
			prev.State = "superseded"
		} else if prev.State == "active" || prev.State == "recovering" {
			if prev.HasActive() {
				prev.State = "draining"
				for _, a := range prev.Attempts {
					if prev.Active(a.Node) && !slices.Contains(prev.DrainNodes, a.Node) {
						prev.DrainNodes = append(prev.DrainNodes, a.Node)
					}
				}
			} else {
				prev.State = "superseded"
			}
		}
	}
	r.Revisions = append(r.Revisions, rev)
	r.CurrentRevision = rev.ID
	r.Description = objective
	return rev.ID, nil
}
func (r *Run) Message(revision, node, recipient, body string, now time.Time) error {
	if revision != r.CurrentRevision {
		return errors.New("cannot steer a superseded revision")
	}
	if recipient != "supervisor" && recipient != "worker" {
		return errors.New("recipient must be supervisor or worker")
	}
	if strings.TrimSpace(body) == "" {
		return errors.New("message is empty")
	}
	rev := r.Current()
	if node != "" {
		if _, ok := rev.Config.Workflow.Nodes[node]; !ok {
			return errors.New("unknown message node")
		}
	}
	rev.Messages = append(rev.Messages, Message{ID: ID("msg"), Node: node, Recipient: recipient, Body: body, CreatedAt: now})
	return nil
}
