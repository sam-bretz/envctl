package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// decision is one line of the Decision Log: who decided what, and why.
type decision struct {
	At     time.Time
	Stage  string
	Actor  string // worker, supervisor, you, or coordinator
	Reason string
}

// correctionPrefix is how the backend records a supervisor rejection on the
// attempt it failed. The log reads it back so a rejection is attributed to
// the supervisor with its correction, not reported as an ordinary failure.
const correctionPrefix = "supervisor correction: "

// decisions derives the Decision Log from state the run already keeps. It
// replaces History, which listed revision and checkpoint IDs: those say what
// exists, not what happened or why.
func decisions(r *workflow.Run) []decision {
	var out []decision
	for i := range r.Revisions {
		rev := &r.Revisions[i]
		plan := rev.Config.Workflow.PlanID()
		if rev.Parent == "" {
			out = append(out, decision{At: rev.CreatedAt, Actor: "coordinator", Reason: "run started: " + firstLine(rev.Objective)})
		} else if parent := r.Revision(rev.Parent); parent != nil {
			out = append(out, decision{At: rev.CreatedAt, Stage: rewoundTo(rev), Actor: "you", Reason: rewindReason(parent, rev)})
		}
		for _, req := range rev.DiscoveredRequirements {
			out = append(out, decision{
				At: planTime(rev, plan), Stage: plan, Actor: "worker",
				Reason: fmt.Sprintf("found that %s needs %s: %s", strings.Join(req.Nodes, ", "), req.Capability, firstLine(req.Reason)),
			})
		}
		for _, a := range rev.Attempts {
			switch {
			case a.State == "failed" && strings.HasPrefix(a.Error, correctionPrefix):
				out = append(out, decision{At: a.UpdatedAt, Stage: a.Node, Actor: "supervisor",
					Reason: fmt.Sprintf("rejected attempt %d: %s", a.Number, firstLine(strings.TrimPrefix(a.Error, correctionPrefix)))})
			case a.State == "failed":
				out = append(out, decision{At: a.UpdatedAt, Stage: a.Node, Actor: "coordinator",
					Reason: fmt.Sprintf("attempt %d failed: %s", a.Number, firstLine(a.Error))})
			case a.Result != nil && a.Result.Review.Accepted:
				out = append(out, decision{At: a.UpdatedAt, Stage: a.Node, Actor: "supervisor",
					Reason: fmt.Sprintf("accepted attempt %d: %s", a.Number, firstLine(a.Result.Review.Summary))})
			}
			if a.Approval != nil {
				out = append(out, decision{At: a.Approval.At, Stage: a.Node, Actor: approver(a.Approval.Actor),
					Reason: fmt.Sprintf("approved attempt %d", a.Number)})
			}
		}
		for _, msg := range rev.Messages {
			stage := msg.Node
			if stage == "" {
				stage = "every stage"
			}
			out = append(out, decision{At: msg.CreatedAt, Stage: stage, Actor: "you",
				Reason: fmt.Sprintf("told the %s: %s [%s]", msg.Recipient, firstLine(msg.Body), rev.MessageStatus(msg))})
		}
	}
	// The captain's log already records past needs-attention causes with their
	// time, which a revision's current Recovery cannot: it only holds the
	// latest one. It exists only when a tracker is configured.
	for _, entry := range r.TrackerLog {
		if entry.Kind == workflow.TrackerKindNeedsAttention {
			out = append(out, decision{At: entry.Occurred, Stage: entry.Node, Actor: "coordinator", Reason: "needed attention: " + firstLine(entry.Detail)})
		}
	}
	slices.SortStableFunc(out, func(a, b decision) int { return a.At.Compare(b.At) })
	// What needs attention now is current, not historical, so it goes last
	// rather than at whatever time a retry happens to be scheduled for.
	if cur := r.Current(); cur != nil && cur.Recovery != nil {
		out = append(out, decision{Actor: "coordinator", Reason: fmt.Sprintf("needs attention now (%s): %s", cur.Recovery.Phase, firstLine(cur.Recovery.Detail))})
	}
	return out
}

// rewoundTo is the first stage the new revision has to run again: the one it
// was rewound to.
func rewoundTo(rev *workflow.Revision) string {
	order, _ := rev.Config.Workflow.Order()
	for _, id := range order {
		if _, kept := rev.Checkpoints[id]; !kept {
			return id
		}
	}
	return ""
}

// rewindReason says what a rewind reran. The stage the person picked is not
// stored on the revision, and changing the objective reruns every stage
// regardless of it, so the first stage that had to run again is the truthful
// and the more useful thing to report.
func rewindReason(parent, rev *workflow.Revision) string {
	reason := "rewound"
	if stage := rewoundTo(rev); stage != "" {
		reason += ", rerunning from " + stage
	}
	var changed []string
	if parent.Objective != rev.Objective {
		changed = append(changed, "the objective")
	}
	if workflow.Digest(parent.Config) != workflow.Digest(rev.Config) {
		changed = append(changed, "the configuration")
	}
	if len(changed) > 0 {
		reason += ", changing " + strings.Join(changed, " and ")
	}
	return reason
}

// planTime places Plan's findings when Plan was accepted, the moment they
// became decisions rather than work in progress.
func planTime(rev *workflow.Revision, plan string) time.Time {
	if cp, ok := rev.Checkpoints[plan]; ok {
		return cp.CreatedAt
	}
	return rev.CreatedAt
}

func approver(actor string) string {
	if actor == "" || actor == "local" {
		return "you"
	}
	return actor
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if r := []rune(line); len(r) > 120 {
		line = string(r[:120]) + "…"
	}
	return line
}

// decisionLog renders the log for the dashboard panel.
func (m Model) decisionLog() string {
	r := m.current()
	if r == nil {
		return "No run selected."
	}
	entries := decisions(r)
	if len(entries) == 0 {
		return "Nothing has been decided yet."
	}
	lines := make([]string, 0, len(entries))
	for _, d := range entries {
		when := "now  "
		if !d.At.IsZero() {
			when = d.At.Local().Format("15:04")
		}
		stage := d.Stage
		if stage == "" {
			stage = "run"
		}
		lines = append(lines, fmt.Sprintf("%s  %-16s %-11s %s", when, stage, d.Actor, d.Reason))
	}
	return strings.Join(lines, "\n")
}

// blocking lists what is stopping a run right now: the readiness problems
// Plan must resolve and any recovery in progress, for the revision and for the
// selected stage's branch runtime. These used to live on the Readiness tab,
// where they sat behind two keypresses on the one screen that matters when a
// run is stuck, so Chat now leads with them. It is empty when nothing blocks.
func blocking(rev *workflow.Revision, node string) []string {
	var out []string
	// One line each: on a small terminal a full list pushed the stage's own
	// content off the screen, which is worse than a truncated alert.
	if problems := rev.ReadinessProblems(time.Now(), rev.Requirements()); len(problems) > 0 {
		out = append(out, fmt.Sprintf("⚠ Plan must resolve %s: %s", plural(len(problems), "readiness problem"), strings.Join(problems, "; ")))
	}
	if rev.Recovery != nil {
		out = append(out, recoveryLine("⚠ Needs attention", rev.Recovery))
	}
	if child := rev.ChildRuntimes[node]; child != nil && child.Recovery != nil {
		out = append(out, recoveryLine("⚠ "+node+"'s branch runtime needs attention", child.Recovery))
	}
	// The Services tab listed every service. Healthy ones are noise while
	// following a run, but one that is down is often why a stage failed, so
	// only those are kept.
	var down []string
	for _, svc := range rev.Runtime.Services {
		if !healthy(svc.State) {
			down = append(down, fmt.Sprintf("%s is %s", svc.Name, svc.State))
		}
	}
	if len(down) > 0 {
		out = append(out, "⚠ Services not healthy in the VM: "+strings.Join(down, "; "))
	}
	return out
}

// healthy reads a Compose service state such as "running/healthy",
// "running", "running/unhealthy" or "exited".
func healthy(state string) bool {
	return strings.HasPrefix(state, "running") && !strings.Contains(state, "unhealthy") && !strings.Contains(state, "starting")
}

func recoveryLine(title string, r *workflow.Recovery) string {
	line := fmt.Sprintf("%s (%s): %s", title, r.Phase, firstLine(r.Detail))
	if !r.RetryAt.IsZero() {
		line += " · retrying after " + r.RetryAt.Local().Format("15:04:05")
	}
	return line
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
