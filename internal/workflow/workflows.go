package workflow

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DefaultWorkflow selects the configuration's `workflow` section.
const DefaultWorkflow = "default"

var templates = map[string]func() Definition{"feature": Feature, "small": Small}

// TemplateNames lists the built-in workflow templates.
func TemplateNames() []string {
	names := make([]string, 0, len(templates))
	for name := range templates {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Small is a three-stage workflow for small changes: Plan, then Build, which
// implements the change and runs the acceptance checks, then an approved
// change that opens the pull request. Build needs checks in the configuration.
func Small() Definition {
	return Definition{Template: "small", Nodes: map[string]Node{
		"plan":            {Kind: "plan", Outputs: []string{"plan"}, Requires: []string{"harness.worker", "harness.supervisor", "runtime.compose", "repositories.readwrite", "publication.pr"}},
		"build":           {Kind: "code", Needs: []string{"plan"}, Outputs: []string{"implementation"}, Writes: []string{"*"}},
		"approved-change": {Kind: "change", Needs: []string{"build"}, Outputs: []string{"change"}, Gate: "human"},
	}}
}

// WorkflowNames lists the workflows a run can select: the default and every
// named workflow, sorted.
func (c Config) WorkflowNames() []string {
	names := []string{DefaultWorkflow}
	for name := range c.Workflows {
		names = append(names, name)
	}
	sort.Strings(names[1:])
	return names
}

// SelectWorkflow returns the configuration a run uses for the named workflow:
// its definition becomes Workflow, and the other named workflows are dropped,
// so a run's stored configuration describes only what it runs.
func (c Config) SelectWorkflow(name string) (Config, error) {
	if name == "" || name == DefaultWorkflow {
		c.Workflows, c.WorkflowName = nil, ""
		c = c.selectTrackerMapping(DefaultWorkflow)
		return c, nil
	}
	def, ok := c.Workflows[name]
	if !ok {
		return c, fmt.Errorf("unknown workflow %q; choose one of: %s", name, strings.Join(c.WorkflowNames(), ", "))
	}
	c.Workflow, c.Workflows, c.WorkflowName = def, nil, name
	c = c.selectTrackerMapping(name)
	return c, c.Validate()
}

func (c Config) selectTrackerMapping(name string) Config {
	if c.Tracker == nil {
		return c
	}
	tracker := Clone(*c.Tracker)
	if mapping, ok := tracker.Mapping[name]; ok {
		tracker.Mapping = map[string]TrackerStatusMapping{name: mapping}
	} else {
		tracker.Mapping = nil
	}
	c.Tracker = &tracker
	return c
}

// verifies reports whether a node produces check evidence an approved
// change can rely on: a QA stage, or any stage with executable checks.
func (n Node) verifies() bool { return n.Kind == "qa" || len(n.Checks) > 0 }

// validateChanges requires every approved change to follow a stage that
// verifies its commits, so a workflow that could never publish is rejected
// before it runs.
func (d Definition) validateChanges() error {
	for id, n := range d.Nodes {
		if n.Kind != "change" {
			continue
		}
		found := false
		for ancestor, a := range d.Nodes {
			if a.verifies() && d.Descendants(ancestor)[id] {
				found = true
			}
		}
		if !found {
			return errors.New("node " + id + " (approved change) needs a QA stage or a stage with checks before it; add checks, for example workflow.nodes.<stage>.checks")
		}
	}
	return nil
}

// DisplayWorkflowName names a run's workflow for people: the selected named
// workflow, or "default".
func (c Config) DisplayWorkflowName() string {
	if c.WorkflowName == "" {
		return DefaultWorkflow
	}
	return c.WorkflowName
}
