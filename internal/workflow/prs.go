package workflow

import (
	"regexp"
	"sort"
)

var pullRequestURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9][A-Za-z0-9_.-]*/[A-Za-z0-9][A-Za-z0-9_.-]*/pull/[0-9]+$`)

// PullRequest is one pull request a run published, keyed as the broker
// recorded it: a repository ID, or <repository>/<submodule path>.
type PullRequest struct {
	Key string `json:"key"`
	URL string `json:"url"`
}

// ApprovedUnpublished returns an approved attempt whose publication has not
// finished, or nil.
func (v *Revision) ApprovedUnpublished() *Attempt {
	for i := range v.Attempts {
		if a := &v.Attempts[i]; a.Approval != nil && a.State == "verifying" {
			return a
		}
	}
	return nil
}

// PullRequests lists the pull requests of the run's newest revision that
// published any, ordered by key. Only GitHub pull request URLs are returned,
// so a caller can open them safely.
func (r *Run) PullRequests() []PullRequest {
	for i := len(r.Revisions) - 1; i >= 0; i-- {
		var out []PullRequest
		for _, cp := range r.Revisions[i].Checkpoints {
			for key, url := range cp.Result.PRs {
				if pullRequestURL.MatchString(url) {
					out = append(out, PullRequest{Key: key, URL: url})
				}
			}
		}
		if len(out) > 0 {
			sort.Slice(out, func(a, b int) bool { return out[a].Key < out[b].Key })
			return out
		}
	}
	return nil
}

// StageStatus is the display state of one stage: its checkpoint, a recovering
// branch runtime, or its latest attempt, naming approved work that is still
// publishing or failed to publish.
func (rev *Revision) StageStatus(node string) string {
	if cp, ok := rev.Checkpoints[node]; ok {
		if cp.HistoricalOnly {
			return "historical"
		}
		return "done"
	}
	if child := rev.ChildRuntimes[node]; child != nil && child.Runtime.OccupiesVM() && child.Recovery != nil {
		return "recovering"
	}
	for i := len(rev.Attempts) - 1; i >= 0; i-- {
		a := rev.Attempts[i]
		if a.Node != node {
			continue
		}
		// Approved work is verified and published in one step; say which.
		if a.State == "verifying" && a.Approval != nil {
			if rev.Recovery != nil && rev.Recovery.Phase == "publication" {
				return "approved, not published"
			}
			return "approved, publishing"
		}
		return a.State
	}
	return "pending"
}
