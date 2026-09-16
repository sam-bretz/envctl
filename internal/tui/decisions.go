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
		// Plan's accepted result is where the scope of the work was decided.
		if cp, ok := rev.Checkpoints[plan]; ok && cp.Result.Summary != "" && rev.Attempt(cp.Attempt) != nil {
			out = append(out, decision{At: cp.CreatedAt, Stage: plan, Actor: "worker", Reason: "scoped the work: " + firstLine(cp.Result.Summary)})
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
		for _, q := range rev.Questions {
			outcome := "waiting for an answer"
			switch q.State {
			case workflow.QuestionAnswered:
				outcome = "answered: " + firstLine(q.Answer)
			case workflow.QuestionFailed:
				outcome = "not answered: " + firstLine(q.Detail)
			}
			out = append(out, decision{At: q.CreatedAt, Stage: q.Node, Actor: approver(q.Asker),
				Reason: fmt.Sprintf("asked the supervisor: %s [%s]", firstLine(q.Text), outcome)})
		}
		// Notes are the decisions no other state keeps: ceiling stops, stall
		// nudges and what publishing did. They replace reading needs-attention
		// causes back from the captain's log, which exists only when a tracker
		// is configured.
		for _, n := range rev.Notes {
			out = append(out, decision{At: n.At, Stage: n.Node, Actor: "coordinator", Reason: noteReason(n)})
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

func noteReason(n workflow.Note) string {
	switch n.Kind {
	case workflow.NoteTokenCeiling:
		return "stopped at the token ceiling: " + firstLine(n.Detail)
	case workflow.NoteAttemptBudget:
		return "stopped retrying: " + firstLine(n.Detail)
	case workflow.NoteStallNudge:
		return firstLine(n.Detail)
	case workflow.NotePublished:
		return "opened the pull request: " + firstLine(n.Detail)
	case workflow.NotePublishFailed:
		return "could not publish: " + firstLine(n.Detail)
	}
	return firstLine(n.Detail)
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

// headerBlockers lists what is stopping a run right now that the run summary
// does not already show: the readiness problems Plan must resolve, and
// recovery on the selected stage's branch runtime. Revision recovery already
// has its own summary line. One line each, so a small terminal keeps room for
// the stage.
func headerBlockers(rev *workflow.Revision, node string) []string {
	var out []string
	if problems := rev.ReadinessProblems(time.Now(), rev.Requirements()); len(problems) > 0 {
		out = append(out, fmt.Sprintf("⚠ Plan must resolve %s: %s", plural(len(problems), "readiness problem"), strings.Join(problems, "; ")))
	}
	if child := rev.ChildRuntimes[node]; child != nil && child.Recovery != nil {
		out = append(out, recoveryLine("⚠ "+node+"'s branch runtime needs attention", child.Recovery))
	}
	return out
}

// serviceLine shows service states when there is a reason to: a preview is
// configured, so the services are what you came to look at, or one is down,
// which is often why a stage failed. Otherwise healthy services are noise.
func serviceLine(rev *workflow.Revision) string {
	var all, down []string
	for _, svc := range rev.Runtime.Services {
		all = append(all, svc.Name+" "+svc.State)
		if !healthy(svc.State) {
			down = append(down, svc.Name+" "+svc.State)
		}
	}
	switch {
	case len(down) > 0:
		return "⚠ Services not healthy: " + strings.Join(down, " · ")
	case rev.Config.Preview != nil && len(all) > 0:
		return "Services: " + strings.Join(all, " · ")
	}
	return ""
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
