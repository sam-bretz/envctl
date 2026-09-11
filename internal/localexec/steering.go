package localexec

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// MaxLiveSteering bounds interrupt-and-resume generations per agent role in
// one attempt. Beyond it, messages reach the supervisor prompt (worker
// steering) or the next attempt, and progress reports the limit.
const MaxLiveSteering = 8

// liveRole is the durable interrupt-and-resume state of one agent role.
// Neither harness accepts input mid-run: a message is delivered by stopping
// the running generation and resuming the same explicit session in a new job.
type liveRole struct {
	Generation int               `json:"generation"`
	Current    *agent.Invocation `json:"current,omitempty"`   // active generation after the frozen invocation
	Interrupt  string            `json:"interrupt,omitempty"` // superseded job that must be terminal before Current starts
	Jobs       []string          `json:"jobs,omitempty"`      // superseded job IDs, oldest first
}

func (r *attemptRecord) live(role string) *liveRole {
	if r.Live == nil {
		r.Live = map[string]*liveRole{}
	}
	if r.Live[role] == nil {
		r.Live[role] = &liveRole{}
	}
	return r.Live[role]
}

// invocation is the role's active generation. The frozen worker invocation
// stays unchanged: the supervisor reviews against the original assignment.
func (r *attemptRecord) invocation(role string) *agent.Invocation {
	if l := r.Live[role]; l != nil && l.Current != nil {
		return l.Current
	}
	if role == "supervisor" {
		return r.Supervisor
	}
	return &r.Worker
}
func (r *attemptRecord) generation(role string) int {
	if l := r.Live[role]; l != nil {
		return l.Generation
	}
	return 0
}
func (r *attemptRecord) session(role string) string {
	if role == "supervisor" {
		return r.SupervisorSession
	}
	return r.Session
}

// jobs lists every guest job of a role in submission order.
func (r *attemptRecord) jobs(role string) []string {
	var ids []string
	if l := r.Live[role]; l != nil {
		ids = append(ids, l.Jobs...)
	}
	if current := r.invocation(role); current != nil {
		ids = append(ids, current.ID)
	}
	return ids
}
func (r *attemptRecord) hasDelivery(message, role string) bool {
	return slices.ContainsFunc(r.Steering, func(d workflow.Delivery) bool { return d.Message == message && d.Role == role })
}

// include records messages carried by a role's frozen prompt (generation 0).
func (r *attemptRecord) include(a engine.Assignment, role string, match func(workflow.Message) bool) {
	for _, m := range a.Revision.Messages {
		if match(m) && !r.hasDelivery(m.ID, role) {
			r.Steering = append(r.Steering, workflow.Delivery{Message: m.ID, Role: role})
		}
	}
}

// markDelivered stamps the active generation's messages once its job exists.
func (r *attemptRecord) markDelivered(role string) bool {
	changed := false
	for i := range r.Steering {
		d := &r.Steering[i]
		if d.Role == role && d.Generation == r.generation(role) && d.At.IsZero() {
			d.At = time.Now().UTC()
			changed = true
		}
	}
	return changed
}
func (r *attemptRecord) delivered() []workflow.Delivery {
	var out []workflow.Delivery
	for _, d := range r.Steering {
		if !d.At.IsZero() {
			out = append(out, d)
		}
	}
	return out
}
func (b *Backend) pendingSteering(a engine.Assignment, r *attemptRecord, role string) []workflow.Message {
	var pending []workflow.Message
	for _, m := range a.Revision.Messages {
		if m.Targets(a.Attempt.Node, role) && !r.hasDelivery(m.ID, role) {
			pending = append(pending, m)
		}
	}
	return pending
}

func steeringPrompt(role string, messages []workflow.Message) string {
	var text strings.Builder
	text.WriteString("The coordinator interrupted your current turn to deliver user steering for this same stage:\n")
	for _, m := range messages {
		fmt.Fprintf(&text, "- %s\n", m.Body)
	}
	if role == "supervisor" {
		text.WriteString("\nRe-assess the worker's result with this steering. Reject the work if it does not address the steering. Return your complete structured assessment.\n")
	} else {
		text.WriteString("\nContinue the same task in the same working directory, incorporating this steering. Your earlier edits are preserved. When finished, return the complete structured result for the whole task, not only for this change.\n")
	}
	return text.String()
}

// steer durably records the next generation before anything is stopped. The
// running job is cancelled by interrupt; a crash between these steps resumes
// from the receipt without submitting a duplicate job or losing a message.
func (b *Backend) steer(a engine.Assignment, r *attemptRecord, role string) (bool, error) {
	pending := b.pendingSteering(a, r, role)
	if len(pending) == 0 || r.session(role) == "" || r.generation(role) >= MaxLiveSteering {
		return false, nil
	}
	l := r.live(role)
	previous := r.invocation(role)
	next := *previous
	next.ID = fmt.Sprintf("%s_%s_%d", a.Attempt.ID, role, l.Generation+1)
	next.Session = r.session(role)
	next.Prompt = steeringPrompt(role, pending)
	l.Jobs = append(l.Jobs, previous.ID)
	l.Interrupt = previous.ID
	l.Generation++
	l.Current = &next
	for _, m := range pending {
		r.Steering = append(r.Steering, workflow.Delivery{Message: m.ID, Role: role, Generation: l.Generation})
	}
	return true, b.save(a, r)
}

// interrupt stops the superseded generation. It reports true while the old
// job may still act; its output keeps draining into the attempt evidence.
func (b *Backend) interrupt(ctx context.Context, a engine.Assignment, r *attemptRecord, role string) (bool, error) {
	l := r.Live[role]
	if l == nil || l.Interrupt == "" {
		return false, nil
	}
	status, err := b.pollJob(ctx, a, r, l.Interrupt)
	if err != nil {
		return true, err
	}
	switch status.State {
	case "pending":
		return true, nil // pollJob reconciled the start intent; cancel once it exists
	case "running", "starting":
		// Cancelling an already finished job is a no-op in the guest journal.
		_, err = b.guest(a).Cancel(ctx, l.Interrupt)
		return true, err
	}
	l.Interrupt = ""
	return false, b.save(a, r)
}

type activityCache struct {
	size  int
	lines []string
}
type activityMemo struct {
	mu   sync.Mutex
	jobs map[string]activityCache
}

// activity parses a job's log only when it has grown since the last poll.
func (b *Backend) activity(id, log string, parse func(string) []string) []string {
	b.memo.mu.Lock()
	defer b.memo.mu.Unlock()
	if b.memo.jobs == nil {
		b.memo.jobs = map[string]activityCache{}
	}
	if c, ok := b.memo.jobs[id]; ok && c.size == len(log) {
		return c.lines
	}
	lines := parse(log)
	b.memo.jobs[id] = activityCache{size: len(log), lines: lines}
	return lines
}

// progress summarizes the attempt's durable phase and redacted recent output.
func (b *Backend) progress(a engine.Assignment, r *attemptRecord) *workflow.Progress {
	node := a.Revision.Config.Workflow.Nodes[a.Attempt.Node]
	p := &workflow.Progress{Phase: r.Phase}
	roleActivity := func(role string) []string {
		h := a.Revision.Config.Agents.Worker
		if role == "supervisor" {
			h = a.Revision.Config.Agents.Supervisor
		}
		harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a))
		if err != nil {
			return nil
		}
		var lines []string
		for i, id := range r.jobs(role) {
			if i > 0 {
				lines = append(lines, fmt.Sprintf("resumed with user steering (resume %d)", i))
			}
			lines = append(lines, b.activity(id, r.Logs[id], harness.Activity)...)
		}
		return lines
	}
	role := ""
	switch r.Phase {
	case "worker":
		role = "worker"
	case "stack":
		p.Detail = "starting Compose services for verification"
	case "checks":
		if r.CheckIndex < len(node.Checks) {
			p.Detail = fmt.Sprintf("check %d of %d: %s", r.CheckIndex+1, len(node.Checks), node.Checks[r.CheckIndex].Name)
			p.Activity = agent.OutputActivity(r.Logs[fmt.Sprintf("%s_check_%d", a.Attempt.ID, r.CheckIndex)], 8)
		} else {
			p.Detail = "capturing data and preparing independent review"
		}
	case "supervisor":
		role = "supervisor"
	}
	if role != "" {
		p.Generation = r.generation(role)
		p.Activity = roleActivity(role)
		if p.Generation >= MaxLiveSteering && len(b.pendingSteering(a, r, role)) > 0 {
			p.Detail = fmt.Sprintf("live steering limit reached (%d resumes); further messages apply to the %s", MaxLiveSteering, map[string]string{"worker": "supervisor review", "supervisor": "next attempt"}[role])
		}
	}
	if len(p.Activity) > 2*workflow.ProgressActivityLines {
		p.Activity = p.Activity[len(p.Activity)-2*workflow.ProgressActivityLines:]
	}
	if len(p.Activity) > 0 {
		p.Activity = strings.Split(string(clean(a, []byte(strings.Join(p.Activity, "\n")))), "\n")
	}
	p.Activity = workflow.BoundActivity(p.Activity)
	return p
}
