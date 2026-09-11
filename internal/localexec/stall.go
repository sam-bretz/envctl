package localexec

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
)

// MaxStallNudges bounds coordinator nudges per agent role in one attempt. A
// stall beyond it fails the attempt, so the engine's retry budget applies.
const MaxStallNudges = 2

// stallState is the durable no-output watch of one agent role. Its times come
// from the coordinator clock and survive backend and coordinator restarts, so
// a restart neither resets nor spuriously trips the window.
type stallState struct {
	Job      string    `json:"job"`                // active job the window belongs to
	Since    time.Time `json:"since"`              // its last output, or when it was first observed
	Extended bool      `json:"extended,omitempty"` // window extended once for a command in flight
	Nudges   []int     `json:"nudges,omitempty"`   // live generations started by stall nudges
	Failing  string    `json:"failing,omitempty"`  // evidence; the stalled job is being stopped
}

func (b *Backend) now() time.Time {
	if b.clock != nil {
		return b.clock()
	}
	return time.Now().UTC()
}

func (r *attemptRecord) stall(role string) *stallState {
	if r.Stall == nil {
		r.Stall = map[string]*stallState{}
	}
	if r.Stall[role] == nil {
		r.Stall[role] = &stallState{}
	}
	return r.Stall[role]
}

// noteOutput restarts the window of the role whose active job produced output.
// Any harness output counts, including events the activity summary omits
// (reasoning, token accounting): only silence is a stall.
func (r *attemptRecord) noteOutput(id string, now time.Time) {
	for _, s := range r.Stall {
		if s.Job == id {
			s.Since, s.Extended = now, false
		}
	}
}

// watchStall runs for a role whose active job is live and has no pending
// steering. It reports true when this poll intervened: a nudge generation was
// recorded, or the stalled job is now being stopped to fail the attempt.
//
// The ladder: after the window with no output, first resume the same session
// with a coordinator nudge; a nudged generation that then stays silent without
// any activity, or a stall after MaxStallNudges nudges, fails the attempt with
// its evidence. A command or tool call still in flight extends the window once,
// because quiet builds and test suites are legitimate.
func (b *Backend) watchStall(a engine.Assignment, r *attemptRecord, role, logs string) (bool, error) {
	current := r.invocation(role)
	s := r.stall(role)
	now := b.now()
	if s.Job != current.ID {
		s.Job, s.Since, s.Extended = current.ID, now, false
		return false, b.save(a, r)
	}
	window := time.Duration(a.Revision.Config.StallSeconds(a.Attempt.Node)) * time.Second
	idle := now.Sub(s.Since)
	if idle < window || (s.Extended && idle < 2*window) {
		return false, nil
	}
	h := a.Revision.Config.Agents.Worker
	if role == "supervisor" {
		h = a.Revision.Config.Agents.Supervisor
	}
	var lines []string
	if harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a)); err == nil {
		lines = b.activity(current.ID, logs, harness.Activity)
	}
	if !s.Extended && agent.CommandInFlight(lines) {
		s.Extended = true
		return false, b.save(a, r)
	}
	evidence := fmt.Sprintf("%s produced no output for %s (stall window %s)", role, short(idle), short(window))
	if len(lines) > 0 {
		evidence += "; last activity: " + lines[len(lines)-1]
	}
	switch {
	case slices.Contains(s.Nudges, r.generation(role)) && len(lines) == 0:
		s.Failing = evidence + "; no response to the coordinator's stall nudge"
	case len(s.Nudges) >= MaxStallNudges:
		s.Failing = fmt.Sprintf("%s; %d stall nudges already sent", evidence, len(s.Nudges))
	case r.session(role) == "":
		s.Failing = evidence + "; the harness never reported a session, so it cannot be resumed"
	case r.generation(role) >= MaxLiveSteering:
		s.Failing = fmt.Sprintf("%s; live resume limit (%d) reached", evidence, MaxLiveSteering)
	default:
		s.Nudges = append(s.Nudges, r.advance(a, role, nudgePrompt(role, idle)))
		return true, b.save(a, r)
	}
	s.Failing = string(clean(a, []byte(s.Failing)))
	return true, b.save(a, r)
}

// stopStalled finishes a failing decision: it stops the stalled job, then
// reports "stalled" once the job is terminal. An agent that completed in the
// meantime keeps its result: finishing is progress.
func (b *Backend) stopStalled(ctx context.Context, a engine.Assignment, r *attemptRecord, role string, status guestjob.Status) (guestjob.Status, bool, error) {
	s := r.Stall[role]
	if pending(status.State) {
		_, err := b.guest(a).Cancel(ctx, r.invocation(role).ID)
		return status, false, err
	}
	if status.State == "completed" {
		s.Failing = ""
		return status, true, b.save(a, r)
	}
	status.State = "stalled"
	return status, true, nil
}

func (r *attemptRecord) stallFailure(role string) string {
	if s := r.Stall[role]; s != nil && s.Failing != "" {
		return "stalled: " + s.Failing
	}
	return ""
}

func nudgePrompt(role string, idle time.Duration) string {
	text := fmt.Sprintf("The coordinator interrupted your current turn: it has seen no output from you for %s. ", short(idle))
	if role == "supervisor" {
		return text + "Report what is blocking your review, then finish it and return your complete structured assessment. Do not wait on long-running commands.\n"
	}
	return text + "Briefly report your status, then continue the same task in the same working directory; your earlier edits are preserved. If a command is hanging, stop waiting on it and use a bounded alternative. If something outside your control blocks you, explain it in the structured result instead of waiting. When finished, return the complete structured result for the whole task.\n"
}

// stallDetail describes the watch for progress once half the window is quiet.
func (b *Backend) stallDetail(a engine.Assignment, r *attemptRecord, role string) string {
	s := r.Stall[role]
	if s == nil {
		return ""
	}
	if s.Failing != "" {
		return "stalled; stopping the agent: " + s.Failing
	}
	var parts []string
	window := time.Duration(a.Revision.Config.StallSeconds(a.Attempt.Node)) * time.Second
	if s.Job == r.invocation(role).ID {
		if idle := b.now().Sub(s.Since); idle >= window/2 {
			limit := window
			note := ""
			if s.Extended {
				limit, note = 2*window, "command in flight, "
			}
			parts = append(parts, fmt.Sprintf("no agent output for %s (%sintervenes at %s)", short(idle.Truncate(time.Second)), note, short(limit)))
		}
	}
	if len(s.Nudges) > 0 {
		parts = append(parts, fmt.Sprintf("stall nudges sent: %d of %d", len(s.Nudges), MaxStallNudges))
	}
	return strings.Join(parts, "; ")
}

// short renders a duration without trailing zero units ("10m", "1h30m").
func short(d time.Duration) string {
	s := d.Round(time.Second).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}
