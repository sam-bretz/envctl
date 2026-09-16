package review

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// Differ fetches a variation's change from retained evidence.
type Differ interface {
	Diff(ctx context.Context, id string, req Request) (Comparison, error)
}

// ComparedVariation is a variation's summary with the size of its change.
type ComparedVariation struct {
	workflow.VariationSummary
	Files       int    `json:"files_changed"`
	Added       int    `json:"lines_added"`
	Removed     int    `json:"lines_removed"`
	Unavailable string `json:"diff_unavailable,omitempty"`
}

func CompareVariations(ctx context.Context, differ Differ, run *workflow.Run) ([]ComparedVariation, error) {
	groups := run.VariationGroups()
	if len(groups) == 0 {
		return nil, errors.New("this run has no variations; a stage proposes them when it sets variations in envctl.yaml")
	}
	var out []ComparedVariation
	// The latest comparison is the one a person is deciding.
	for _, s := range run.CompareVariations(groups[len(groups)-1]) {
		c := ComparedVariation{VariationSummary: s}
		if s.DiffNode != "" {
			diff, err := differ.Diff(ctx, run.ID, Request{Revision: s.Revision, Node: s.DiffNode})
			if err != nil {
				c.Unavailable = err.Error()
			} else {
				c.Files, c.Added, c.Removed = Stat(diff)
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func VariationsText(compared []ComparedVariation) string {
	var b strings.Builder
	out := &b
	for i, v := range compared {
		if i > 0 {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%s  [%s]  %s\n", v.Name, v.Status, v.Revision)
		if v.Rationale != "" {
			fmt.Fprintf(out, "  why: %s\n", firstLineOf(v.Rationale))
		}
		for _, s := range v.Stages {
			fmt.Fprintf(out, "  %s: %s\n", s.Node, firstLineOf(s.Summary))
		}
		switch {
		case v.Unavailable != "":
			fmt.Fprintf(out, "  change: unavailable (%s)\n", v.Unavailable)
		case v.DiffNode != "":
			fmt.Fprintf(out, "  change: %d files, +%d -%d  (envctl run diff <run> --revision %s --node %s)\n", v.Files, v.Added, v.Removed, v.Revision, v.DiffNode)
		}
		repos := make([]string, 0, len(v.Commits))
		for repo := range v.Commits {
			repos = append(repos, repo)
		}
		sort.Strings(repos)
		for _, repo := range repos {
			fmt.Fprintf(out, "  commit %s: %s\n", repo, shortSHA(v.Commits[repo]))
		}
		for _, c := range v.Checks {
			outcome := "passed"
			if !c.Passed {
				outcome = fmt.Sprintf("failed (exit %d)", c.ExitCode)
			}
			fmt.Fprintf(out, "  check %s/%s: %s\n", c.Node, c.Name, outcome)
		}
		fmt.Fprintf(out, "  tokens: %s\n", workflow.FormatTokens(v.Usage.Tokens()))
	}
	return b.String()
}

func firstLineOf(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return line
}

func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
