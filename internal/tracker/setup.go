package tracker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// Team is a tracker team the credential can see.
type Team struct {
	ID   string
	Key  string
	Name string
}

// State is one of a team's workflow states. Type is the tracker's own
// classification (Linear: backlog, unstarted, started, completed, canceled),
// which is what lets the wizard suggest a mapping without guessing from names.
type State struct {
	Name string
	Type string
}

// Directory is what the setup wizard reads from a tracker. It never changes
// an issue: every method is a read.
type Directory interface {
	Viewer(ctx context.Context) (string, error)
	Teams(ctx context.Context) ([]Team, error)
	States(ctx context.Context, teamID string) ([]State, error)
}

// NewDirectory opens a read-only directory for a configured tracker.
func NewDirectory(cfg workflow.TrackerConfig) (Directory, error) {
	if cfg.Provider != "linear" {
		return nil, fmt.Errorf("unsupported tracker %q; only linear is supported", cfg.Provider)
	}
	client, err := newLinearClient(cfg)
	if err != nil {
		// Returning client here would hand back a non-nil interface holding
		// a nil pointer.
		return nil, err
	}
	return client, nil
}

// Stage is what the wizard needs to know about a workflow stage to suggest
// a mapping for it.
type Stage struct {
	ID string
	// Gated stages wait for a person, which is the natural moment for an
	// issue to move into review.
	Gated bool
}

// Stages describes a workflow's stages in run order.
func Stages(def workflow.Definition) []Stage {
	order, _ := def.Order()
	out := make([]Stage, 0, len(order))
	for _, id := range order {
		out = append(out, Stage{ID: id, Gated: def.Nodes[id].Gate == "human"})
	}
	return out
}

// DefaultMapping suggests a mapping from a team's states so that accepting
// every default gives one that works. It chooses by state type, which the
// tracker defines, and uses names only to separate states of the same type,
// such as Linear's In Progress and In Review, which are both "started".
//
// Rewound maps back to the working state because the configuration requires
// that a rewind after Done not leave the issue Done; mapping it to nothing
// would do exactly that.
func DefaultMapping(states []State, stages []Stage) workflow.TrackerStatusMapping {
	var m workflow.TrackerStatusMapping
	working := pick(states, "started", func(s State) bool { return !mentions(s, "review") })
	// Review has no fallback. A team without a review state should leave
	// approval unmapped, not move a waiting run back into the working state.
	review := only(states, "started", func(s State) bool { return mentions(s, "review") })
	m.RunStarted = working
	m.Rewound = working
	m.PRPublished = pick(states, "completed", nil)
	m.Cancelled = pick(states, "canceled", nil)
	if review != "" {
		for _, stage := range stages {
			if stage.Gated {
				if m.AwaitingApproval == nil {
					m.AwaitingApproval = map[string]string{}
				}
				m.AwaitingApproval[stage.ID] = review
			}
		}
	}
	return m
}

// pick returns the first state of a type that satisfies prefer, falling back
// to the first state of that type at all.
func pick(states []State, kind string, prefer func(State) bool) string {
	fallback := ""
	for _, s := range states {
		if !strings.EqualFold(s.Type, kind) {
			continue
		}
		if prefer == nil || prefer(s) {
			return s.Name
		}
		if fallback == "" {
			fallback = s.Name
		}
	}
	return fallback
}

// only returns the first state of a type that satisfies want, or nothing.
func only(states []State, kind string, want func(State) bool) string {
	for _, s := range states {
		if strings.EqualFold(s.Type, kind) && want(s) {
			return s.Name
		}
	}
	return ""
}

func mentions(s State, word string) bool {
	return strings.Contains(strings.ToLower(s.Name), word)
}

// Preview lists the transitions a typical run would make, in the order it
// would make them, so the person sees the effect rather than only the YAML.
func Preview(m workflow.TrackerStatusMapping, stages []Stage) []string {
	var out []string
	add := func(event, status string) {
		if status != "" {
			out = append(out, fmt.Sprintf("%s → %s", event, status))
		}
	}
	add("run starts", m.RunStarted)
	for _, stage := range stages {
		add(stage.ID+" starts", m.StageStarted[stage.ID])
		add(stage.ID+" waits for approval", m.AwaitingApproval[stage.ID])
		add(stage.ID+" is accepted", m.StageAccepted[stage.ID])
	}
	add("approved", m.Approved)
	add("pull request published", m.PRPublished)
	return out
}

// TrackerBlock renders the complete tracker: section the wizard writes. The
// credential is only ever the file reference; the token itself never reaches
// the configuration.
func TrackerBlock(provider, credentialFile string, mapping map[string]workflow.TrackerStatusMapping) (string, error) {
	if !strings.HasPrefix(credentialFile, "/") {
		return "", errors.New("the credential must be an absolute file path")
	}
	cfg := workflow.TrackerConfig{Provider: provider, Credential: "file:" + credentialFile, Mapping: mapping}
	raw, err := yaml.Marshal(map[string]workflow.TrackerConfig{"tracker": cfg})
	if err != nil {
		return "", err
	}
	return strings.TrimRight(string(raw), "\n"), nil
}

// SpliceTracker writes a tracker: block into a configuration, replacing an
// existing one so that re-running the wizard edits the mapping instead of
// adding a second. It splices text rather than re-encoding the document, so
// every byte outside the tracker block, comments included, is kept. Comments
// inside the replaced tracker block are not.
func SpliceTracker(raw []byte, block string) ([]byte, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("envctl.yaml is not a mapping")
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	replacement := strings.Split(block, "\n")
	root := doc.Content[0]
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value != "tracker" {
			continue
		}
		value := root.Content[i+1]
		if value.Style&yaml.FlowStyle != 0 {
			return nil, errors.New("tracker: is written on one line; rewrite it as an indented block, or edit the mapping by hand")
		}
		start, end := root.Content[i].Line-1, lastLine(value)
		return []byte(strings.Join(slices.Concat(lines[:start], replacement, lines[end:]), "\n") + "\n"), nil
	}
	return []byte(strings.Join(slices.Concat(lines, []string{""}, replacement), "\n") + "\n"), nil
}

// lastLine is the line, counted from one, of the deepest content in a node.
func lastLine(n *yaml.Node) int {
	last := n.Line
	for _, child := range n.Content {
		if deeper := lastLine(child); deeper > last {
			last = deeper
		}
	}
	return last
}
