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
