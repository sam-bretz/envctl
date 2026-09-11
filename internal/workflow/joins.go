package workflow

import (
	"fmt"
	"slices"
)

// validateJoinOwnership propagates possible source identities through the
// graph. A writable stage owns a new identity; read-only stages forward their
// input. Divergence must have an explicit merge owner before a run is admitted.
func (c Config) validateJoinOwnership() error {
	order, err := c.Workflow.Order()
	if err != nil {
		return err
	}
	for _, repo := range c.Repositories {
		origins := map[string]string{}
		for _, id := range order {
			n := c.Workflow.Nodes[id]
			inputs := map[string]bool{}
			for _, parent := range n.Needs {
				inputs[origins[parent]] = true
			}
			if len(inputs) > 1 && (n.Join == nil || n.Join.Repositories[repo.ID] == "") {
				return fmt.Errorf("node %s joins potentially different %s commits; declare join.repositories.%s and writable merge ownership", id, repo.ID, repo.ID)
			}
			origin := "baseline"
			if len(n.Needs) > 0 {
				origin = origins[n.Needs[0]]
			}
			if slices.Contains(n.Writes, "*") || slices.Contains(n.Writes, repo.ID) {
				origin = "node:" + id
			}
			origins[id] = origin
		}
	}
	for _, id := range order {
		n := c.Workflow.Nodes[id]
		if len(n.Needs) < 2 {
			continue
		}
		for _, dataset := range c.Data.Datasets {
			if n.Join == nil || n.Join.Datasets[dataset.ID] == "" {
				return fmt.Errorf("node %s must explicitly select join.datasets.%s", id, dataset.ID)
			}
		}
	}
	return nil
}

// MergeInputs derives the mandatory ancestry from accepted direct inputs,
// never from a worker's account of what it merged.
func (r *Revision) MergeInputs(node string) (map[string]map[string]string, error) {
	n := r.Config.Workflow.Nodes[node]
	if n.Join == nil || len(n.Join.Repositories) == 0 {
		return nil, nil
	}
	result := map[string]map[string]string{}
	for repo := range n.Join.Repositories {
		result[repo] = map[string]string{}
		for _, parent := range n.Needs {
			cp, ok := r.Checkpoints[parent]
			if !ok || cp.HistoricalOnly || !shaRE.MatchString(cp.Result.Commits[repo]) {
				return nil, fmt.Errorf("merge input %s lacks an accepted immutable commit for %s", parent, repo)
			}
			result[repo][parent] = cp.Result.Commits[repo]
		}
	}
	return result, nil
}
