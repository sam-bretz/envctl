package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// chatEntry is one line of a stage's conversation.
type chatEntry struct {
	At time.Time
	// Who said it: you, worker, supervisor or coordinator. To is set for
	// what you sent, and names who it went to.
	Who, To string
	// Kind is steer, ask, answer, thinking, summary, review, approval,
	// progress or status.
	Kind   string
	Text   string
	Status string
}

// chatThread is a stage's conversation in the order it happened. The panel
// used to print attempts in one block and messages in another, which read as
// two logs rather than a conversation; progress summaries are now entries in
// the same thread as what you said.
func chatThread(rev *workflow.Revision, node string) []chatEntry {
	var out []chatEntry
	for _, a := range rev.Attempts {
		if a.Node != node {
			continue
		}
		out = append(out, chatEntry{At: a.StartedAt, Who: "coordinator", Kind: "status", Text: fmt.Sprintf("attempt %d started", a.Number)})
		if a.Result != nil && a.Result.Summary != "" {
			out = append(out, chatEntry{At: a.UpdatedAt, Who: "worker", Kind: "summary", Text: a.Result.Summary})
		}
		switch {
		case a.State == "failed" && strings.HasPrefix(a.Error, correctionPrefix):
			out = append(out, chatEntry{At: a.UpdatedAt, Who: "supervisor", Kind: "review", Text: "rejected: " + strings.TrimPrefix(a.Error, correctionPrefix)})
		case a.State == "failed":
			out = append(out, chatEntry{At: a.UpdatedAt, Who: "coordinator", Kind: "status", Text: fmt.Sprintf("attempt %d failed: %s", a.Number, a.Error)})
		case a.Result != nil && a.Result.Review.Accepted:
			out = append(out, chatEntry{At: a.UpdatedAt, Who: "supervisor", Kind: "review", Text: "accepted: " + a.Result.Review.Summary})
		}
		if a.State == "awaiting-approval" {
			out = append(out, chatEntry{At: a.UpdatedAt, Who: "coordinator", Kind: "status", Text: "waiting for your approval · a to approve"})
		}
		if a.Approval != nil {
			out = append(out, chatEntry{At: a.Approval.At, Who: approver(a.Approval.Actor), Kind: "approval",
				Text: fmt.Sprintf("approved attempt %d at %s", a.Number, a.Approval.At.Local().Format("2006-01-02 15:04"))})
		}
		if p := a.Progress; a.State == "running" && p != nil {
			// "live:" says this is happening now, and keeps a phase named
			// "worker" from reading as "worker: worker".
			text := "live: " + p.Phase
			if p.Generation > 0 {
				text += fmt.Sprintf(" (resume %d)", p.Generation)
			}
			if p.Detail != "" {
				text += " — " + p.Detail
			}
			if len(p.Activity) > 0 {
				text += "\n" + strings.Join(p.Activity, "\n")
			}
			out = append(out, chatEntry{At: p.UpdatedAt, Who: "worker", Kind: "progress", Text: text})
		}
	}
	// A rewind carries earlier stages' checkpoints into the new revision
	// without the attempts that produced them. Without this, an inherited
	// stage's accepted result would show nothing at all in Chat.
	if cp, ok := rev.Checkpoints[node]; ok && rev.Attempt(cp.Attempt) == nil {
		out = append(out, chatEntry{At: cp.CreatedAt, Who: "coordinator", Kind: "status", Text: "result carried over from an earlier revision"})
		if cp.Result.Summary != "" {
			out = append(out, chatEntry{At: cp.CreatedAt, Who: "worker", Kind: "summary", Text: cp.Result.Summary})
		}
		if cp.Result.Review.Summary != "" {
			out = append(out, chatEntry{At: cp.CreatedAt, Who: "supervisor", Kind: "review", Text: "accepted: " + cp.Result.Review.Summary})
		}
		if cp.Approval != nil {
			out = append(out, chatEntry{At: cp.Approval.At, Who: approver(cp.Approval.Actor), Kind: "approval",
				Text: "approved at " + cp.Approval.At.Local().Format("2006-01-02 15:04")})
		}
	}
	for _, msg := range rev.Messages {
		if msg.Node == "" || msg.Node == node {
			out = append(out, chatEntry{At: msg.CreatedAt, Who: "you", To: msg.Recipient, Kind: "steer", Text: msg.Body, Status: rev.MessageStatus(msg)})
		}
	}
	for _, q := range rev.Questions {
		if q.Node != node {
			continue
		}
		out = append(out, chatEntry{At: q.CreatedAt, Who: "you", To: "supervisor", Kind: "ask", Text: q.Text})
		switch q.State {
		case workflow.QuestionAnswered:
			out = append(out, chatEntry{At: q.AnsweredAt, Who: "supervisor", Kind: "answer", Text: q.Answer})
		case workflow.QuestionFailed:
			out = append(out, chatEntry{At: q.AnsweredAt, Who: "supervisor", Kind: "answer", Text: "could not answer: " + q.Detail})
		default:
			// Placed a moment after the question so it always sorts beneath it.
			out = append(out, chatEntry{At: q.CreatedAt.Add(time.Nanosecond), Who: "supervisor", Kind: "thinking"})
		}
	}
	slices.SortStableFunc(out, func(a, b chatEntry) int { return a.At.Compare(b.At) })
	return out
}

var thinkingFrames = []string{"thinking", "thinking.", "thinking..", "thinking..."}

// chat renders the Chat panel for the selected stage.
func (m Model) chat() string {
	rev := m.viewRevision()
	node := m.nodeID()
	agents := rev.Config.NodeAgents(node)
	head := []string{fmt.Sprintf("Worker model: %s · Supervisor model: %s", formatModelTUI(agents.Worker.Model), formatModelTUI(agents.Supervisor.Model))}
	// A stage's dependencies are the part of the old Graph tab worth keeping.
	if deps := rev.Config.Workflow.Nodes[node].Needs; len(deps) > 0 {
		head = append(head, "Runs after: "+strings.Join(deps, ", "))
	}
	if len(rev.Config.Plugins) > 0 {
		var attached []string
		for _, p := range rev.Config.Plugins {
			attached = append(attached, p.ID+"@"+p.Version)
		}
		head = append(head, "Plugins: "+strings.Join(attached, ", ")+" (p add or replace, P remove; reopens Plan)")
	}

	thread := chatThread(rev, node)
	if len(thread) == 0 {
		// On an empty stage what to do next matters more than which model is
		// configured, and a small terminal only has room for a few lines.
		return strings.Join(append([]string{"No stage messages yet. i to steer the " + m.Recipient + ", ? to ask the supervisor."}, head...), "\n")
	}
	lines := append(head, "")
	for _, e := range thread {
		lines = append(lines, m.chatLine(e))
	}
	if artifacts := m.chatArtifacts(rev, node); artifacts != "" {
		lines = append(lines, "", artifacts)
	}
	return strings.Join(lines, "\n")
}

func (m Model) chatLine(e chatEntry) string {
	when := "     "
	if !e.At.IsZero() {
		when = e.At.Local().Format("15:04")
	}
	who := e.Who
	if e.To != "" {
		// Saying which kind a message was keeps an ask from being mistaken
		// for an instruction that changed the work.
		who += " → " + e.To + " (" + e.Kind + ")"
	}
	text := e.Text
	if e.Kind == "thinking" {
		text = thinkingFrames[m.Frame%len(thinkingFrames)]
	}
	if e.Status != "" {
		text += " [" + e.Status + "]"
	}
	indent := strings.Repeat(" ", len(when)+2)
	return when + "  " + who + ": " + strings.ReplaceAll(strings.TrimRight(text, "\n"), "\n", "\n"+indent)
}

// chatArtifacts lists what the selected stage produced, so , . and o keep
// working now that the Checkpoint tab is gone.
func (m Model) chatArtifacts(rev *workflow.Revision, node string) string {
	artifacts := []workflow.Artifact{}
	title := "Artifacts (,/. select · o open):"
	if cp, ok := rev.Checkpoints[node]; ok {
		artifacts = cp.Result.Artifacts
	} else {
		for _, a := range rev.Attempts {
			if a.Node == node && a.State == "awaiting-approval" && a.Result != nil {
				artifacts = a.Result.Artifacts
				title = "Artifacts awaiting approval:"
			}
		}
	}
	if len(artifacts) == 0 {
		return ""
	}
	lines := []string{title}
	for i, a := range artifacts {
		marker := " "
		if i == m.ArtifactIndex {
			marker = ">"
		}
		lines = append(lines, fmt.Sprintf("%s %s · %s · %d bytes", marker, a.Name, a.MediaType, a.Size))
	}
	return strings.Join(lines, "\n")
}
