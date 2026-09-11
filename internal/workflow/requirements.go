package workflow

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Requirement is planner-authored scope. It can add an admission obligation,
// never claim readiness or remove a configured obligation.
type Requirement struct {
	Capability string   `json:"capability"`
	Nodes      []string `json:"nodes"`
	Reason     string   `json:"reason"`
}

var capabilityName = regexp.MustCompile(`^[a-z][a-z0-9_-]*(\.[a-z][a-z0-9_-]*)*$`)

// RecordPlanResult validates the supervised work product before it can extend
// the readiness inventory, even when its newly discovered connection is absent.
func (r *Revision) RecordPlanResult(node string, result Result, now time.Time) error {
	if r.Config.Workflow.Nodes[node].Kind == "plan" {
		if err := r.validateResultEvidence(node, result, now, false); err != nil {
			return err
		}
	}
	return r.RecordPlanRequirements(node, result.Requirements)
}

func ValidateRequirements(definition Definition, requirements []Requirement) error {
	if len(requirements) > 256 {
		return errors.New("Plan requirement inventory exceeds 256 entries")
	}
	seen := map[string]bool{}
	for _, requirement := range requirements {
		if len(requirement.Capability) > 128 || !capabilityName.MatchString(requirement.Capability) || seen[requirement.Capability] {
			return errors.New("Plan requirements must have unique valid capability names")
		}
		seen[requirement.Capability] = true
		if strings.TrimSpace(requirement.Reason) == "" || len(requirement.Reason) > 4096 || len(requirement.Nodes) == 0 {
			return errors.New("Plan requirement needs affected nodes and a bounded explanation")
		}
		nodes := map[string]bool{}
		for _, node := range requirement.Nodes {
			if _, ok := definition.Nodes[node]; !ok || nodes[node] {
				return fmt.Errorf("Plan requirement %s references an unknown or duplicate node", requirement.Capability)
			}
			nodes[node] = true
		}
	}
	return nil
}

// RecordPlanRequirements retains validated obligations across readiness waits
// and retries. Removing an obligation requires an amended workflow revision.
func (r *Revision) RecordPlanRequirements(node string, requirements []Requirement) error {
	if r.Config.Workflow.Nodes[node].Kind != "plan" {
		if len(requirements) > 0 {
			return errors.New("only Plan can declare workflow requirements")
		}
		return nil
	}
	if err := ValidateRequirements(r.Config.Workflow, requirements); err != nil {
		return err
	}
	merged := Clone(r.DiscoveredRequirements)
	for _, requirement := range requirements {
		index := slices.IndexFunc(merged, func(prior Requirement) bool { return prior.Capability == requirement.Capability })
		if index < 0 {
			merged = append(merged, Clone(requirement))
			continue
		}
		for _, node := range requirement.Nodes {
			if !slices.Contains(merged[index].Nodes, node) {
				merged[index].Nodes = append(merged[index].Nodes, node)
			}
		}
		slices.Sort(merged[index].Nodes)
	}
	if len(merged) > 256 {
		return errors.New("accumulated Plan inventory exceeds 256 entries; amend the workflow")
	}
	slices.SortFunc(merged, func(a, b Requirement) int { return strings.Compare(a.Capability, b.Capability) })
	r.DiscoveredRequirements = merged
	return nil
}
