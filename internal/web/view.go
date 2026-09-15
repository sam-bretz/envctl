package web

import (
	"slices"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// The page never receives a run's configuration: credential references and
// plugin settings stay in the coordinator. It gets these views instead, with
// every state the dashboard derives already computed.

type State struct {
	Root  string    `json:"root"`
	Runs  []RunView `json:"runs"`
	Error string    `json:"error,omitempty"`
	At    time.Time `json:"at"`
}

type RunView struct {
	ID              string                 `json:"id"`
	Name            string                 `json:"name"`
	Description     string                 `json:"description"`
	TaskRef         string                 `json:"task_ref,omitempty"`
	Owner           string                 `json:"owner"`
	Priority        int                    `json:"priority"`
	Version         int64                  `json:"version"`
	CurrentRevision string                 `json:"current_revision"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
	State           string                 `json:"state"`
	Lane            string                 `json:"lane"` // attention, running or finished
	Attention       string                 `json:"attention,omitempty"`
	Usage           UsageView              `json:"usage"`
	PullRequests    []workflow.PullRequest `json:"pull_requests"`
	Revisions       []RevisionView         `json:"revisions"`
}

type UsageView struct {
	Summary   string  `json:"summary"`
	Fraction  float64 `json:"fraction"`
	Tokens    int64   `json:"tokens"`
	Limit     int64   `json:"limit"`
	CostUSD   float64 `json:"cost_usd"`
	Estimated bool    `json:"estimated,omitempty"`
}

type RevisionView struct {
	ID             string                `json:"id"`
	Parent         string                `json:"parent,omitempty"`
	FromCheckpoint string                `json:"from_checkpoint,omitempty"`
	Objective      string                `json:"objective"`
	State          string                `json:"state"`
	Current        bool                  `json:"current"`
	Workflow       string                `json:"workflow"`
	CreatedAt      time.Time             `json:"created_at"`
	Recovery       *workflow.Recovery    `json:"recovery,omitempty"`
	Blocked        *BlockedView          `json:"blocked,omitempty"`
	Stalled        string                `json:"stalled,omitempty"`
	Runtime        workflow.RuntimeState `json:"runtime"`
	Children       []ChildView           `json:"children,omitempty"`
	Readiness      ReadinessView         `json:"readiness"`
	Stages         []StageView           `json:"stages"`
	Messages       []MessageView         `json:"messages"`
	Repositories   []RepositoryView      `json:"repositories"`
}

// BlockedView is approved work whose publication keeps failing.
type BlockedView struct {
	Node   string `json:"node"`
	Detail string `json:"detail"`
}

type ChildView struct {
	Node     string                `json:"node"`
	Runtime  workflow.RuntimeState `json:"runtime"`
	Recovery *workflow.Recovery    `json:"recovery,omitempty"`
}

type ReadinessView struct {
	Requirements []string               `json:"requirements"`
	Problems     []string               `json:"problems"`
	Probes       []workflow.Probe       `json:"probes"`
	Discovered   []workflow.Requirement `json:"discovered,omitempty"`
	Plugins      []PluginView           `json:"plugins,omitempty"`
}

type PluginView struct {
	ID       string   `json:"id"`
	Version  string   `json:"version"`
	Digest   string   `json:"digest,omitempty"`
	Provides []string `json:"provides,omitempty"`
	Requires []string `json:"requires,omitempty"`
}

type StageView struct {
	ID         string               `json:"id"`
	Kind       string               `json:"kind"`
	Needs      []string             `json:"needs"`
	Gate       string               `json:"gate,omitempty"`
	Status     string               `json:"status"`
	Depth      int                  `json:"depth"`
	Checks     []string             `json:"checks,omitempty"`
	Limits     LimitView            `json:"limits"`
	Models     ModelView            `json:"models"`
	Attempts   []workflow.Attempt   `json:"attempts"`
	Checkpoint *workflow.Checkpoint `json:"checkpoint,omitempty"`
	Awaiting   *AwaitingView        `json:"awaiting,omitempty"`
}

type LimitView struct {
	Attempts       int `json:"attempts"`
	MaxAttempts    int `json:"max_attempts"`
	AttemptSeconds int `json:"attempt_seconds"`
	StallSeconds   int `json:"stall_seconds"`
}

// ModelView names each role's effective model; empty is the harness default.
type ModelView struct {
	Worker     string `json:"worker"`
	Supervisor string `json:"supervisor"`
}

// AwaitingView is the exact result a person approves.
type AwaitingView struct {
	Attempt    string          `json:"attempt"`
	WorkDigest string          `json:"work_digest"`
	Result     workflow.Result `json:"result"`
}

type MessageView struct {
	workflow.Message
	Status string `json:"status"`
}

type RepositoryView struct {
	ID           string `json:"id"`
	URL          string `json:"url"`
	Ref          string `json:"ref"`
	SourcePin    string `json:"source_pin,omitempty"`
	OutputBranch string `json:"output_branch"`
	Destination  string `json:"destination,omitempty"`
}

func runView(r workflow.Run, now time.Time) RunView {
	cur := r.Current()
	u := r.Usage()
	v := RunView{
		ID: r.ID, Name: r.Name, Description: r.Description, TaskRef: r.TaskRef, Owner: r.Owner, Priority: r.Priority,
		Version: r.Version, CurrentRevision: r.CurrentRevision, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt,
		PullRequests: r.PullRequests(),
		Usage:        UsageView{Summary: r.UsageSummary(), Fraction: r.UsageFraction(), Tokens: u.Tokens(), CostUSD: u.CostUSD, Estimated: u.Estimated},
	}
	if v.PullRequests == nil {
		v.PullRequests = []workflow.PullRequest{}
	}
	if cur != nil {
		v.State = cur.State
		v.Usage.Limit = cur.Config.RunTokenLimit()
	}
	for i := range r.Revisions {
		v.Revisions = append(v.Revisions, revisionView(&r.Revisions[i], r.Revisions[i].ID == r.CurrentRevision, now))
	}
	v.Lane, v.Attention = lane(cur)
	return v
}

// lane sorts a run by what it needs from a person.
func lane(v *workflow.Revision) (string, string) {
	if v == nil {
		return "finished", ""
	}
	order, _ := v.Config.Workflow.Order()
	for _, id := range order {
		for _, a := range v.Attempts {
			if a.Node == id && a.State == "awaiting-approval" {
				return "attention", id + " is waiting for your approval"
			}
		}
	}
	if a := v.ApprovedUnpublished(); a != nil && v.Recovery != nil && v.Recovery.Phase == "publication" {
		return "attention", "The pull request was not opened"
	}
	if problem := stalledOn(v); problem != "" {
		return "attention", "Blocked: " + problem
	}
	switch v.State {
	case "needs-attention":
		reason := "Needs attention"
		if v.Recovery != nil {
			reason += ": " + v.Recovery.Detail
		}
		return "attention", reason
	case "completed", "cancelled", "superseded":
		return "finished", ""
	}
	return "running", ""
}

// stalledOn is the first readiness problem holding back a revision that has
// finished planning and has nothing running, so no stage can start.
func stalledOn(v *workflow.Revision) string {
	if v.State != "active" || v.HasActive() {
		return ""
	}
	if _, planned := v.Checkpoints[v.Config.Workflow.PlanID()]; !planned {
		return ""
	}
	// Only a failed check counts: missing or expired evidence is refreshed by
	// the coordinator on its own and would flash a false alarm.
	required := v.Requirements()
	for _, p := range v.Readiness {
		if !p.Passed && slices.Contains(required, p.Capability) {
			return p.Capability + ": " + p.Detail
		}
	}
	return ""
}

func revisionView(v *workflow.Revision, current bool, now time.Time) RevisionView {
	out := RevisionView{
		ID: v.ID, Parent: v.Parent, FromCheckpoint: v.FromCheckpoint, Objective: v.Objective, State: v.State,
		Current: current, Workflow: v.Config.DisplayWorkflowName(), CreatedAt: v.CreatedAt, Recovery: v.Recovery, Runtime: v.Runtime,
		Stages: []StageView{}, Messages: []MessageView{}, Repositories: []RepositoryView{},
	}
	if a := v.ApprovedUnpublished(); a != nil && v.Recovery != nil && v.Recovery.Phase == "publication" {
		out.Blocked = &BlockedView{Node: a.Node, Detail: v.Recovery.Detail}
	}
	if current {
		out.Stalled = stalledOn(v)
	}
	order, _ := v.Config.Workflow.Order()
	for _, node := range order {
		if child := v.ChildRuntimes[node]; child != nil {
			out.Children = append(out.Children, ChildView{Node: node, Runtime: child.Runtime, Recovery: child.Recovery})
		}
	}
	requirements := v.Requirements()
	out.Readiness = ReadinessView{Requirements: requirements, Problems: v.ReadinessProblems(now, requirements), Probes: v.Readiness, Discovered: v.DiscoveredRequirements}
	if out.Readiness.Problems == nil {
		out.Readiness.Problems = []string{}
	}
	if out.Readiness.Probes == nil {
		out.Readiness.Probes = []workflow.Probe{}
	}
	for _, p := range v.Config.Plugins {
		out.Readiness.Plugins = append(out.Readiness.Plugins, PluginView{ID: p.ID, Version: p.Version, Digest: p.Digest, Provides: p.Provides, Requires: p.Requires})
	}

	depth := map[string]int{}
	for _, id := range order {
		n := v.Config.Workflow.Nodes[id]
		for _, need := range n.Needs {
			depth[id] = max(depth[id], depth[need]+1)
		}
		l := v.Config.NodeLimits(id)
		agents := v.Config.NodeAgents(id)
		s := StageView{
			ID: id, Kind: n.Kind, Needs: n.Needs, Gate: n.Gate, Status: v.StageStatus(id), Depth: depth[id],
			Limits:   LimitView{MaxAttempts: l.MaxAttempts, AttemptSeconds: l.AttemptSeconds, StallSeconds: v.Config.StallSeconds(id)},
			Models:   ModelView{Worker: agents.Worker.Model, Supervisor: agents.Supervisor.Model},
			Attempts: []workflow.Attempt{},
		}
		if s.Needs == nil {
			s.Needs = []string{}
		}
		for _, c := range n.Checks {
			s.Checks = append(s.Checks, c.Name)
		}
		for _, a := range v.Attempts {
			if a.Node != id {
				continue
			}
			s.Attempts = append(s.Attempts, a)
			s.Limits.Attempts++
			if a.State == "awaiting-approval" && a.Result != nil {
				s.Awaiting = &AwaitingView{Attempt: a.ID, WorkDigest: a.Result.WorkDigest(), Result: *a.Result}
			}
		}
		if cp, ok := v.Checkpoints[id]; ok {
			s.Checkpoint = &cp
		}
		out.Stages = append(out.Stages, s)
	}
	for _, m := range v.Messages {
		out.Messages = append(out.Messages, MessageView{Message: m, Status: v.MessageStatus(m)})
	}
	for _, repo := range v.Config.Repositories {
		rv := RepositoryView{ID: repo.ID, URL: repo.URL, Ref: repo.Ref, SourcePin: v.SourcePins[repo.ID], OutputBranch: workflow.OutputBranch(repo, v.ID)}
		if repo.Publication != nil {
			rv.Destination = repo.Publication.Repository + " → " + repo.Publication.Base
		}
		out.Repositories = append(out.Repositories, rv)
	}
	return out
}
