package localexec

import (
	"context"
	"fmt"
	"slices"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/repository"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func joinInputID(a engine.Assignment, parent string) string {
	return a.Attempt.ID + "_input_" + workflow.Digest(parent)[:16]
}

func (b *Backend) prepareJoin(ctx context.Context, a engine.Assignment) error {
	parents, err := a.Revision.MergeInputs(a.Attempt.Node)
	if err != nil || len(parents) == 0 {
		return err
	}
	// Hydrate each input in an independent worktree. Besides supplying both
	// histories to Git, this makes nested modules and LFS payloads available
	// offline for conflict resolution. None of these paths is a live writer.
	for _, parent := range a.Revision.Config.Workflow.Nodes[a.Attempt.Node].Needs {
		pins := workflow.Clone(a.Revision.SourcePins)
		if pins == nil {
			pins = map[string]string{}
		}
		for repo, pin := range a.Revision.Checkpoints[parent].Result.Commits {
			pins[repo] = pin
		}
		if _, err := b.worktrees(ctx, a, joinInputID(a, parent), pins); err != nil {
			return fmt.Errorf("merge input %s could not be restored: %w", parent, err)
		}
	}
	return nil
}

func (b *Backend) verifyJoin(ctx context.Context, a engine.Assignment, result *workflow.Result) error {
	parents, err := a.Revision.MergeInputs(a.Attempt.Node)
	if err != nil {
		return err
	}
	result.MergeParents = parents
	for repo, inputs := range parents {
		raw, err := b.Store.Artifact(result.Sources[repo].Digest)
		if err != nil {
			return err
		}
		pins := []string{}
		for _, pin := range inputs {
			pins = append(pins, pin)
		}
		slices.Sort(pins)
		if err = repository.VerifyMerge(ctx, raw, result.Commits[repo], pins); err != nil {
			return fmt.Errorf("merge %s: %w", repo, err)
		}
	}
	return nil
}
