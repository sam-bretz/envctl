package tracker

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// Render reconstructs entry's comment body from run's own retained,
// immutable history: checkpoints, attempts and earlier posted entries are
// never mutated by later rewinds, so this is safe to call at any later
// time, including after a coordinator restart. Render performs no
// redaction and no size bounding; those are the delivery loop's job,
// applied uniformly regardless of kind.
func Render(run *workflow.Run, entry workflow.TrackerLogEntry) (string, error) {
	rev := run.Revision(entry.Revision)
	if rev == nil {
		return "", fmt.Errorf("tracker log entry references unknown revision %s", entry.Revision)
	}
	switch entry.Kind {
	case workflow.TrackerKindStageCompleted:
		return renderStageCompleted(run, rev, entry)
	case workflow.TrackerKindAwaitingApproval:
		return renderAwaitingApproval(rev, entry)
	case workflow.TrackerKindApproved:
		return renderApproved(rev, entry)
	case workflow.TrackerKindRewound:
		return fmt.Sprintf("New revision `%s`. %s", entry.Revision, entry.Detail), nil
	case workflow.TrackerKindNeedsAttention:
		return "Needs attention: " + entry.Detail, nil
	default:
		return "", fmt.Errorf("unknown tracker log entry kind %q", entry.Kind)
	}
}

func renderStageCompleted(run *workflow.Run, rev *workflow.Revision, entry workflow.TrackerLogEntry) (string, error) {
	cp, ok := rev.Checkpoints[entry.Node]
	if !ok {
		return "", fmt.Errorf("tracker log entry references unresolved checkpoint %s", entry.Node)
	}
	att := rev.Attempt(cp.Attempt)
	number := 0
	if att != nil {
		number = att.Number
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## %s — attempt %d\n", entry.Node, number)
	if s := strings.TrimSpace(cp.Result.Summary); s != "" {
		fmt.Fprintf(&b, "\n**Summary**\n%s\n", s)
	}
	if s := strings.TrimSpace(cp.Result.Review.Summary); s != "" {
		fmt.Fprintf(&b, "\n**Review**\n%s\n", s)
	}
	if len(cp.Result.Checks) > 0 {
		b.WriteString("\n**Checks**\n")
		for _, c := range cp.Result.Checks {
			status := "fail"
			if c.Passed {
				status = "pass"
			}
			fmt.Fprintf(&b, "- `%s`: %s (exit %d)\n", c.Name, status, c.ExitCode)
		}
	}
	if len(cp.Result.Commits) > 0 {
		b.WriteString("\n**Commits**\n")
		repos := sortedKeys(cp.Result.Commits)
		for _, repo := range repos {
			fmt.Fprintf(&b, "- %s: `%s`\n", repo, cp.Result.Commits[repo])
		}
	}
	if len(cp.Result.PRs) > 0 {
		b.WriteString("\n**Pull requests**\n")
		repos := sortedKeys(cp.Result.PRs)
		for _, repo := range repos {
			fmt.Fprintf(&b, "- %s: %s\n", repo, cp.Result.PRs[repo])
		}
	}
	fmt.Fprintf(&b, "\n**Usage so far**\n%s\n", run.UsageSummary())
	if so := soFar(run, entry); so != "" {
		fmt.Fprintf(&b, "\n**So far**\n%s", so)
	}
	return b.String(), nil
}

// soFar builds a short recap from every earlier stage_completed entry
// already posted for this run, in the order they were enqueued.
func soFar(run *workflow.Run, entry workflow.TrackerLogEntry) string {
	var b strings.Builder
	for _, e := range run.TrackerLog {
		if e.ID == entry.ID {
			break
		}
		if e.Kind != workflow.TrackerKindStageCompleted || e.Status != "posted" {
			continue
		}
		rev := run.Revision(e.Revision)
		if rev == nil {
			continue
		}
		cp, ok := rev.Checkpoints[e.Node]
		if !ok {
			continue
		}
		summary := firstLine(cp.Result.Summary, 140)
		fmt.Fprintf(&b, "- `%s`: %s\n", e.Node, summary)
	}
	return b.String()
}

func firstLine(s string, limit int) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return s
}

func renderAwaitingApproval(rev *workflow.Revision, entry workflow.TrackerLogEntry) (string, error) {
	att := rev.Attempt(entry.Attempt)
	if att == nil {
		return "", errors.New("tracker log entry references unknown attempt")
	}
	line := fmt.Sprintf("Stage %s (attempt %d) is awaiting human approval.", entry.Node, att.Number)
	if att.Result != nil && strings.TrimSpace(att.Result.Summary) != "" {
		line += "\n\n**Summary**\n" + att.Result.Summary
	}
	return line, nil
}

func renderApproved(rev *workflow.Revision, entry workflow.TrackerLogEntry) (string, error) {
	att := rev.Attempt(entry.Attempt)
	if att == nil || att.Approval == nil {
		return "", errors.New("tracker log entry references an attempt with no approval")
	}
	return fmt.Sprintf("Stage %s (attempt %d) was approved by %s.", entry.Node, att.Number, att.Approval.Actor), nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
