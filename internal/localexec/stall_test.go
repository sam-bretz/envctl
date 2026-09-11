package localexec

import (
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
)

const sessionLine = `{"type":"thread.started","thread_id":"` + steerSession + `"}` + "\n"

func agentLine(text string) string {
	return `{"type":"item.completed","item":{"id":"` + text + `","type":"agent_message","text":"` + text + `"}}` + "\n"
}

// stallFixture runs a read-only stage so a failed worker needs no source
// capture, with a controllable coordinator clock.
func stallFixture(t *testing.T) (*Backend, engine.Assignment, *steerGuest, *time.Time) {
	t.Helper()
	b, a := fixture(t)
	a.Attempt.Node = "plan"
	t.Setenv("ENVCTL_STEER_KEY", "fixture-secret-key")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_STEER_KEY"
	a.Revision.Config.Agents.Supervisor.Credential = "env:ENVCTL_STEER_KEY"
	a.Attempt.Number, a.Attempt.State = 1, "running"
	g := &steerGuest{jobs: map[string]*fakeJob{}}
	b.Provider = g
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	b.clock = func() time.Time { return now }
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "worker", Cursors: map[string]int64{}, Logs: map[string]string{},
		Worker: agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: "/work/envctl/repos/app", Prompt: "plan the task", Schema: map[string]any{"type": "object"}, TimeoutSeconds: 3600}}
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	return b, a, g, &now
}

// restart recreates the backend with the same store, guest and clock.
func restart(b *Backend, g *steerGuest) *Backend {
	next := New(b.Store)
	next.Provider, next.clock = g, b.clock
	return next
}

func TestStallNudgesOnceThenFailsWhenTheResumedAgentStaysSilent(t *testing.T) {
	b, a, g, now := stallFixture(t)
	first := a.Attempt.ID + "_worker"
	poll(t, b, a) // start
	g.jobs[first].output = sessionLine + agentLine("reading the task")
	poll(t, b, a)
	*now = now.Add(9 * time.Minute)
	if o := poll(t, b, a); o.State != "running" || !strings.Contains(o.Progress.Detail, "no agent output for 9m (intervenes at 10m)") {
		t.Fatal("quiet period not visible before intervention", o.Progress)
	}
	// Coordinator restart mid-window keeps the timing: it neither resets the
	// window nor trips it early.
	b = restart(b, g)
	if r := receipt(t, b, a); r.Live["worker"] != nil {
		t.Fatal("nudged before the window elapsed")
	}
	*now = now.Add(2 * time.Minute)
	poll(t, b, a)
	r := receipt(t, b, a)
	live, stall := r.Live["worker"], r.Stall["worker"]
	if live == nil || live.Generation != 1 || live.Interrupt != first || len(stall.Nudges) != 1 || stall.Nudges[0] != 1 {
		t.Fatalf("stall nudge intent not durable: %+v %+v", live, stall)
	}
	if !strings.Contains(live.Current.Prompt, "no output from you for 11m") || live.Current.Session != steerSession {
		t.Fatal("nudge must resume the same session with the coordinator's question", live.Current.Prompt)
	}
	if len(r.Steering) != 0 {
		t.Fatal("a coordinator nudge was recorded as user steering", r.Steering)
	}
	// Restart between the durable nudge intent and the interruption.
	b = restart(b, g)
	poll(t, b, a)
	if g.jobs[first].state != "cancelled" || g.jobs[first+"_1"] != nil {
		t.Fatal("nudged generation started before the stalled job stopped")
	}
	poll(t, b, a)
	nudged := g.jobs[first+"_1"]
	if nudged == nil || nudged.submits != 1 || !strings.Contains(strings.Join(nudged.args, " "), steerSession) {
		t.Fatal("nudge did not resume the explicit session", nudged)
	}
	o := poll(t, b, a)
	if !strings.Contains(strings.Join(o.Progress.Activity, "\n"), "resumed with a coordinator stall nudge (resume 1)") || !strings.Contains(o.Progress.Detail, "stall nudges sent: 1 of 2") {
		t.Fatal("nudge not visible in progress", o.Progress)
	}
	// The resumed agent emits harness metadata but no activity for a full window.
	nudged.output = sessionLine
	poll(t, b, a)
	*now = now.Add(11 * time.Minute)
	o = poll(t, b, a)
	if o.State != "running" || nudged.cancels != 1 || !strings.Contains(o.Progress.Detail, "stalled; stopping the agent") {
		t.Fatal("second stall did not stop the silent agent visibly", o)
	}
	o = poll(t, b, a)
	if o.State != "failed" || !strings.Contains(o.Detail, "stalled: worker produced no output for 11m") || !strings.Contains(o.Detail, "no response to the coordinator's stall nudge") {
		t.Fatal("stalled attempt did not fail with evidence", o)
	}
	if len(g.jobs) != 2 {
		t.Fatal("stall ladder looped", len(g.jobs))
	}
}

func TestStallWindowExtendsForCommandsAndNudgesAreBounded(t *testing.T) {
	b, a, g, now := stallFixture(t)
	first := a.Attempt.ID + "_worker"
	poll(t, b, a)
	g.jobs[first].output = sessionLine + `{"type":"item.started","item":{"id":"c1","type":"command_execution","command":"pytest -q"}}` + "\n"
	poll(t, b, a)
	*now = now.Add(11 * time.Minute)
	o := poll(t, b, a)
	if receipt(t, b, a).Live["worker"] != nil || !receipt(t, b, a).Stall["worker"].Extended {
		t.Fatal("a quiet command in flight was interrupted without an extension")
	}
	if !strings.Contains(o.Progress.Detail, "command in flight, intervenes at 20m") {
		t.Fatal("extension not visible", o.Progress)
	}
	*now = now.Add(10 * time.Minute)
	poll(t, b, a)
	if r := receipt(t, b, a); r.generation("worker") != 1 {
		t.Fatal("extended window did not end in a nudge")
	}
	// Each nudged generation that responds and then stalls again is nudged
	// until the bound; the next stall fails.
	for gen := 1; gen <= MaxStallNudges; gen++ {
		poll(t, b, a) // stop previous
		poll(t, b, a) // start this generation
		job := g.jobs[first+"_"+string(rune('0'+gen))]
		if job == nil {
			t.Fatal("generation not started", gen)
		}
		job.output = agentLine("status report")
		poll(t, b, a)
		*now = now.Add(11 * time.Minute)
		poll(t, b, a)
	}
	r := receipt(t, b, a)
	if len(r.Stall["worker"].Nudges) != MaxStallNudges || !strings.Contains(r.Stall["worker"].Failing, "2 stall nudges already sent") {
		t.Fatalf("nudges not bounded: %+v", r.Stall["worker"])
	}
	// An agent that finishes while being stopped keeps its result.
	g.jobs[r.invocation("worker").ID].state = "completed"
	_, _ = b.Poll(t.Context(), a) // the fixture result is not a valid proposal
	if r := receipt(t, b, a); r.Stall["worker"].Failing != "" || len(g.results) != 1 {
		t.Fatal("completion while stopping was discarded", r.Stall["worker"])
	}
}

func TestUserSteeringTakesPrecedenceAndStallsWithoutSessionFail(t *testing.T) {
	b, a, g, now := stallFixture(t)
	first := a.Attempt.ID + "_worker"
	poll(t, b, a)
	g.jobs[first].output = sessionLine
	poll(t, b, a)
	// A stalled agent with pending user steering is resumed with the user's
	// message, not a nudge, and the new generation restarts the window.
	*now = now.Add(15 * time.Minute)
	message(&a, "msg_user", "worker", "prefer the simpler approach")
	poll(t, b, a)
	r := receipt(t, b, a)
	if r.generation("worker") != 1 || len(r.Stall["worker"].Nudges) != 0 || !strings.Contains(r.Live["worker"].Current.Prompt, "prefer the simpler approach") {
		t.Fatal("pending user steering did not take precedence over the nudge")
	}
	poll(t, b, a)
	poll(t, b, a)
	*now = now.Add(5 * time.Minute)
	poll(t, b, a)
	if receipt(t, b, a).generation("worker") != 1 {
		t.Fatal("the steered generation inherited the old window")
	}

	// An agent that never reported a session cannot be resumed: fail.
	b2, a2, g2, now2 := stallFixture(t)
	poll(t, b2, a2)
	poll(t, b2, a2)
	*now2 = now2.Add(11 * time.Minute)
	poll(t, b2, a2)
	poll(t, b2, a2)
	if o := poll(t, b2, a2); o.State != "failed" || !strings.Contains(o.Detail, "never reported a session") {
		t.Fatal("unresumable stall did not fail", o)
	}
	if len(g2.jobs) != 1 || g2.jobs[a2.Attempt.ID+"_worker"].cancels != 1 {
		t.Fatal("the unresumable agent was not stopped exactly once", len(g2.jobs))
	}
}
