package workflow

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// NotChosen is the terminal state of a variation someone decided against. It
// is kept for review and releases its runtime like a superseded revision, but
// unlike one it records that it was a real, finished candidate.
const NotChosen = "not-chosen"

// AsProposed names the variation that continues the approach the worker
// actually took, alongside the alternatives the supervisor proposed.
const AsProposed = "as proposed"

// Variant marks a revision as one of several competing approaches built from
// the same stage. Every revision in the comparison shares Group.
type Variant struct {
	Group     string `json:"group"`
	Node      string `json:"node"`
	Name      string `json:"name"`
	Rationale string `json:"rationale,omitempty"`
	Chosen    bool   `json:"chosen,omitempty"`
}

// Undecided reports whether a revision is a variation still waiting for a
// person to choose between it and its siblings. An undecided variation may
// run every stage except an approved change, so none can publish before a
// choice is made.
func (r *Revision) Undecided() bool {
	return r.Variant != nil && !r.Variant.Chosen && r.State != NotChosen
}

// Parked reports whether a finished variation has given up its VM while it
// waits to be chosen. It stays active, so it is not mistaken for a retired
// one, but holds no VM and must not be counted against limits.vms.
func (r *Revision) Parked() bool {
	return r.State == "active" && r.Undecided() && r.Runtime.State == "stopped" && !r.Runtime.OccupiesVM() && !r.HasChildVMs()
}

// Schedulable reports whether a revision may start work: the current one, or
// an undecided variation running beside it.
func (r *Run) Schedulable(revision string) bool {
	if revision == r.CurrentRevision {
		return true
	}
	rev := r.Revision(revision)
	return rev != nil && rev.Undecided()
}

// Branch turns an accepted stage's proposals into variations. The revision
// that was accepted continues as the approach already taken; each proposal
// gets a sibling revision restored from before that stage, with the
// alternative as steering for its worker. Nothing is superseded and the
// current revision does not change until a person chooses.
func (r *Run) Branch(revision, node string, proposals []Variation, now time.Time) error {
	source := r.Revision(revision)
	if source == nil || revision != r.CurrentRevision {
		return errors.New("variations branch only from the current revision")
	}
	if source.Variant != nil {
		// A variation of a variation would multiply the work past the bound the
		// configuration set, so alternatives are proposed once per run branch.
		return errors.New("a variation cannot branch again")
	}
	n, ok := source.Config.Workflow.Nodes[node]
	if !ok || n.Variations < MinVariations {
		return fmt.Errorf("stage %s does not allow variations", node)
	}
	if len(proposals) < MinVariations || len(proposals) > n.Variations {
		return fmt.Errorf("stage %s allows %d to %d variations, not %d", node, MinVariations, n.Variations, len(proposals))
	}
	if _, accepted := source.Checkpoints[node]; !accepted {
		return fmt.Errorf("stage %s has no accepted result to vary", node)
	}

	group := ID("variation")
	rationale := source.Checkpoints[node].Result.Review.Summary
	// Build every sibling before touching r.Revisions: appending can move the
	// slice and leave source pointing at a stale copy.
	invalid := source.Config.Workflow.Descendants(node)
	invalid[node] = true
	order, _ := source.Config.Workflow.Order()
	siblings := make([]Revision, 0, len(proposals))
	for _, p := range proposals {
		sibling := Revision{
			ID: ID("rev"), Parent: source.ID, Objective: source.Objective, Config: Clone(source.Config),
			State: "queued", Checkpoints: map[string]Checkpoint{}, SourcePins: Clone(source.SourcePins),
			DiscoveredRequirements: Clone(source.DiscoveredRequirements), Messages: Clone(source.Messages), CreatedAt: now,
			Variant: &Variant{Group: group, Node: node, Name: p.Name, Rationale: p.Rationale},
		}
		for id, cp := range source.Checkpoints {
			if !invalid[id] && !cp.HistoricalOnly {
				sibling.Checkpoints[id] = Clone(cp)
			}
		}
		for _, id := range order {
			if cp, ok := sibling.Checkpoints[id]; ok {
				sibling.FromCheckpoint = cp.ID
			}
		}
		sibling.Messages = append(sibling.Messages, Message{
			ID: ID("msg"), Node: node, Recipient: "worker", CreatedAt: now,
			Body: "Build this alternative approach rather than the one taken before, so it can be compared side by side: " + p.Name + ". " + p.Rationale,
		})
		siblings = append(siblings, sibling)
	}
	source.Variant = &Variant{Group: group, Node: node, Name: AsProposed, Rationale: rationale}
	r.Revisions = append(r.Revisions, siblings...)
	return nil
}

// Variations lists a group's revisions in the order they were created.
func (r *Run) Variations(group string) []*Revision {
	var out []*Revision
	for i := range r.Revisions {
		if v := r.Revisions[i].Variant; v != nil && v.Group == group {
			out = append(out, &r.Revisions[i])
		}
	}
	return out
}

// VariationGroups lists the run's comparisons, oldest first.
func (r *Run) VariationGroups() []string {
	var groups []string
	for _, rev := range r.Revisions {
		if rev.Variant != nil && !slices.Contains(groups, rev.Variant.Group) {
			groups = append(groups, rev.Variant.Group)
		}
	}
	return groups
}

// Finished reports whether a variation has run every stage it may run before
// a choice: everything except its approved changes.
func (r *Revision) Finished() bool {
	for id, n := range r.Config.Workflow.Nodes {
		if n.Kind == "change" {
			continue
		}
		if _, ok := r.Checkpoints[id]; !ok {
			return false
		}
	}
	return true
}

// Choose continues one variation to its approved change and retires the rest.
// The chosen one becomes the current revision. The others stop, keep their
// checkpoints for review, release their runtimes and are never published.
func (r *Run) Choose(revision string, now time.Time) error {
	chosen := r.Revision(revision)
	if chosen == nil || chosen.Variant == nil {
		return errors.New("that revision is not a variation")
	}
	if !chosen.Undecided() {
		return errors.New("that comparison has already been decided")
	}
	if !chosen.Finished() {
		var pending []string
		for id, n := range chosen.Config.Workflow.Nodes {
			if _, ok := chosen.Checkpoints[id]; !ok && n.Kind != "change" {
				pending = append(pending, id)
			}
		}
		slices.Sort(pending)
		return fmt.Errorf("%s has not finished %s yet; choose once its stages are done so it can be compared", chosen.Variant.Name, strings.Join(pending, ", "))
	}
	group := chosen.Variant.Group
	var others []string
	for _, rev := range r.Variations(group) {
		if rev.ID != revision {
			others = append(others, rev.Variant.Name)
		}
	}
	chosen.AddNote(Note{At: now, Kind: NoteChosen, Node: chosen.Variant.Node,
		Detail: fmt.Sprintf("chose %s over %s", chosen.Variant.Name, strings.Join(others, ", "))})
	for _, rev := range r.Variations(group) {
		if rev.ID == revision {
			rev.Variant.Chosen = true
			continue
		}
		rev.State = NotChosen
		for j := range rev.Attempts {
			a := &rev.Attempts[j]
			if slices.Contains([]string{"preparing", "running", "verifying", "awaiting-approval"}, a.State) {
				a.State = "cancelled"
				a.UpdatedAt = now
			}
		}
	}
	// A variation parked while it waited gave up its VM. Queue it again so the
	// coordinator provisions one and restores its source from its checkpoints,
	// the same way a rewound revision resumes.
	if !chosen.Runtime.Ready {
		chosen.State = "queued"
		chosen.Runtime = RuntimeState{}
	}
	r.CurrentRevision = revision
	r.Description = chosen.Objective
	return nil
}

// VariationStage is one stage of a variation as a person compares it.
type VariationStage struct {
	Node    string `json:"node"`
	Summary string `json:"summary,omitempty"`
	Review  string `json:"review,omitempty"`
}

// VariationCheck is a check a variation's stage ran, and against which
// commits, so results from different variations are never confused.
type VariationCheck struct {
	Node     string `json:"node"`
	Name     string `json:"name"`
	Passed   bool   `json:"passed"`
	ExitCode int    `json:"exit_code,omitempty"`
}

// VariationSummary is everything a person needs to compare one variation
// with its siblings: what it did, why it was proposed, the tests it ran and
// what it cost. The diff comes from retained evidence and is fetched
// separately, since it needs the artifact store.
type VariationSummary struct {
	Revision  string            `json:"revision"`
	Name      string            `json:"name"`
	Rationale string            `json:"rationale,omitempty"`
	Status    string            `json:"status"`
	Finished  bool              `json:"finished"`
	Stages    []VariationStage  `json:"stages"`
	Commits   map[string]string `json:"commits,omitempty"`
	// DiffNode is the last stage with source commits: comparing its
	// checkpoint against the revision's source pins gives the variation's
	// whole change.
	DiffNode string           `json:"diff_node,omitempty"`
	Checks   []VariationCheck `json:"checks"`
	Usage    Usage            `json:"usage"`
}

// Status words for a variation.
const (
	VariationBuilding = "building"
	VariationReady    = "ready to choose"
	VariationChosen   = "chosen"
	VariationRetired  = "not chosen"
)

// CompareVariations summarizes a comparison's variations side by side.
func (r *Run) CompareVariations(group string) []VariationSummary {
	var out []VariationSummary
	for _, rev := range r.Variations(group) {
		s := VariationSummary{Revision: rev.ID, Name: rev.Variant.Name, Rationale: rev.Variant.Rationale, Finished: rev.Finished(), Stages: []VariationStage{}, Checks: []VariationCheck{}, Usage: rev.Usage()}
		switch {
		case rev.State == NotChosen:
			s.Status = VariationRetired
		case rev.Variant.Chosen:
			s.Status = VariationChosen
		case s.Finished:
			s.Status = VariationReady
		default:
			s.Status = VariationBuilding
		}
		order, _ := rev.Config.Workflow.Order()
		for _, id := range order {
			if rev.Config.Workflow.Nodes[id].Kind == "change" {
				continue
			}
			cp, ok := rev.Checkpoints[id]
			if !ok {
				continue
			}
			s.Stages = append(s.Stages, VariationStage{Node: id, Summary: cp.Result.Summary, Review: cp.Result.Review.Summary})
			if len(cp.Result.Commits) > 0 {
				s.Commits, s.DiffNode = Clone(cp.Result.Commits), id
			}
			for _, c := range cp.Result.Checks {
				s.Checks = append(s.Checks, VariationCheck{Node: id, Name: c.Name, Passed: c.Passed, ExitCode: c.ExitCode})
			}
		}
		out = append(out, s)
	}
	return out
}

// Usage is what one revision's agents used: its attempts and the questions
// asked of it. Run.Usage sums every revision.
func (r *Revision) Usage() Usage {
	var total Usage
	for _, u := range r.ProbeUsage {
		total = total.Add(u)
	}
	for _, a := range r.Attempts {
		switch {
		case a.Usage != nil:
			total = total.Add(*a.Usage)
		case a.Progress != nil && a.Progress.Usage != nil:
			total = total.Add(*a.Progress.Usage)
		}
	}
	for _, q := range r.Questions {
		if q.Usage != nil {
			total = total.Add(*q.Usage)
		}
	}
	return total
}

// FindVariation resolves a variation by revision ID or by name, within the
// run's undecided comparison.
func (r *Run) FindVariation(ref string) (*Revision, error) {
	ref = strings.TrimSpace(ref)
	var matches []*Revision
	for i := range r.Revisions {
		rev := &r.Revisions[i]
		if rev.Variant == nil {
			continue
		}
		if rev.ID == ref || strings.EqualFold(rev.Variant.Name, ref) {
			matches = append(matches, rev)
		}
	}
	switch len(matches) {
	case 0:
		return nil, fmt.Errorf("no variation named %q", ref)
	case 1:
		return matches[0], nil
	}
	// A name can repeat across comparisons; prefer the one still undecided.
	for _, m := range matches {
		if m.Undecided() {
			return m, nil
		}
	}
	return nil, fmt.Errorf("%q names more than one variation; use its revision ID", ref)
}
