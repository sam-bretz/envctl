package localexec

import (
	"context"
	"encoding/json"
	"io"
	"path"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const steerSession = "01a08f59-7073-7a83-b959-867ca896ce48"

type fakeJob struct {
	state, output, input string
	args                 []string
	submits, cancels     int
}

// steerGuest simulates the guest journal: one process per job ID, idempotent
// submission, cancellation that is a no-op for finished jobs, and results.
type steerGuest struct {
	vm.Provider
	jobs    map[string]*fakeJob
	results []string // result files read, in order
}

func (g *steerGuest) Exec(_ context.Context, _ string, c vm.Command) error {
	if len(c.Args) == 5 && c.Args[0] == "sudo" && c.Args[3] == "cat" {
		id := path.Base(path.Dir(c.Args[4]))
		g.results = append(g.results, id)
		_, err := io.WriteString(c.Stdout, `{"summary":"done","accepted":true}`)
		return err
	}
	if len(c.Args) != 4 || c.Args[2] != guestjob.RunnerPath {
		return nil // harness home/schema preparation
	}
	var req guestjob.Request
	var cursor struct {
		Cursor int64 `json:"cursor"`
	}
	raw, _ := io.ReadAll(c.Stdin)
	if err := json.Unmarshal(raw, &req); err != nil {
		return err
	}
	_ = json.Unmarshal(raw, &cursor)
	job := g.jobs[req.ID]
	status := guestjob.Status{ID: req.ID, State: "missing"}
	switch c.Args[3] {
	case "submit":
		if job == nil {
			job = &fakeJob{state: "running", input: req.Input, args: req.Args}
			g.jobs[req.ID] = job
		}
		job.submits++
	case "cancel":
		if job == nil {
			return json.NewEncoder(c.Stdout).Encode(guestjob.Status{State: "error", Detail: "cannot cancel an unknown job"})
		}
		job.cancels++
		if job.state == "running" {
			job.state = "cancelled"
		}
	}
	if job != nil {
		status.State = job.state
		if cursor.Cursor < int64(len(job.output)) {
			status.Output = job.output[cursor.Cursor:]
		}
		status.Cursor = int64(len(job.output))
	}
	return json.NewEncoder(c.Stdout).Encode(status)
}

func steerFixture(t *testing.T) (*Backend, engine.Assignment, *steerGuest) {
	t.Helper()
	b, a := fixture(t)
	t.Setenv("ENVCTL_STEER_KEY", "fixture-secret-key")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_STEER_KEY"
	a.Revision.Config.Agents.Supervisor.Credential = "env:ENVCTL_STEER_KEY"
	a.Attempt.Number, a.Attempt.State = 1, "running"
	g := &steerGuest{jobs: map[string]*fakeJob{}}
	b.Provider = g
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "worker", Cursors: map[string]int64{}, Logs: map[string]string{},
		Worker: agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: "/work/envctl/repos/app", Prompt: "implement the task", Schema: map[string]any{"type": "object"}, TimeoutSeconds: 600}}
	r.include(a, "worker", func(m workflow.Message) bool { return m.Recipient == "worker" })
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	return b, a, g
}
func poll(t *testing.T, b *Backend, a engine.Assignment) engine.Observation {
	t.Helper()
	o, err := b.Poll(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func message(a *engine.Assignment, id, recipient, body string) {
	a.Revision.Messages = append(a.Revision.Messages, workflow.Message{ID: id, Node: a.Attempt.Node, Recipient: recipient, Body: body, CreatedAt: time.Now()})
}
func receipt(t *testing.T, b *Backend, a engine.Assignment) *attemptRecord {
	t.Helper()
	r, err := b.load(a)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLiveSteeringInterruptsAndResumesTheSameSession(t *testing.T) {
	b, a, g := steerFixture(t)
	// Arrived before Start froze the prompt, as Start records it.
	message(&a, "msg_before", "worker", "use the existing client")
	frozen := receipt(t, b, a)
	frozen.include(a, "worker", func(m workflow.Message) bool { return m.Recipient == "worker" })
	if err := b.save(a, frozen); err != nil {
		t.Fatal(err)
	}
	first := a.Attempt.ID + "_worker"
	if o := poll(t, b, a); o.State != "running" || g.jobs[first] == nil {
		t.Fatal("frozen worker not started")
	}
	// Session not yet known: a new message waits; nothing is interrupted.
	message(&a, "msg_live", "worker", "also keep the API stable")
	message(&a, "msg_other", "supervisor", "check the migration")
	poll(t, b, a)
	if r := receipt(t, b, a); r.Live["worker"] != nil || g.jobs[first].cancels != 0 {
		t.Fatal("steered before the explicit session was known")
	}
	g.jobs[first].output = `{"type":"thread.started","thread_id":"` + steerSession + `"}` + "\n" + `{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"working"}}` + "\n"
	o := poll(t, b, a)
	r := receipt(t, b, a)
	live := r.Live["worker"]
	if live == nil || live.Generation != 1 || live.Interrupt != first || live.Current.ID != first+"_1" || live.Current.Session != steerSession {
		t.Fatalf("steering intent not durable before interruption: %+v", live)
	}
	if !strings.Contains(live.Current.Prompt, "also keep the API stable") || strings.Contains(live.Current.Prompt, "use the existing client") || strings.Contains(live.Current.Prompt, "check the migration") {
		t.Fatal("resumed prompt must carry exactly the undelivered worker steering", live.Current.Prompt)
	}
	if r.Worker.ID != first || r.Worker.Prompt != "implement the task" {
		t.Fatal("frozen assignment changed; the supervisor would review the wrong instructions")
	}
	if len(o.Delivered) != 1 || o.Delivered[0].Message != "msg_before" || o.Delivered[0].Generation != 0 {
		t.Fatal("intent reported as delivered before its job existed", o.Delivered)
	}
	// Coordinator crash between the durable intent and cancellation.
	b = New(b.Store)
	b.Provider = g
	poll(t, b, a)
	if g.jobs[first].cancels != 1 || g.jobs[first].state != "cancelled" || g.jobs[first+"_1"] != nil {
		t.Fatal("new generation started before the old one was stopped")
	}
	o = poll(t, b, a)
	next := g.jobs[first+"_1"]
	if next == nil || next.submits != 1 || !strings.Contains(strings.Join(next.args, " "), "resume") || !strings.Contains(strings.Join(next.args, " "), steerSession) || next.input != live.Current.Prompt {
		t.Fatal("new generation did not resume the explicit session", next)
	}
	if len(o.Delivered) != 2 || o.Delivered[1].Message != "msg_live" || o.Delivered[1].Generation != 1 {
		t.Fatal("live delivery not reported", o.Delivered)
	}
	if o.Progress == nil || o.Progress.Phase != "worker" || o.Progress.Generation != 1 || !strings.Contains(strings.Join(o.Progress.Activity, "\n"), "agent: working") {
		t.Fatal("progress does not show the resumed worker", o.Progress)
	}
	// Recreation after the resumed job started does not submit it again.
	b = New(b.Store)
	b.Provider = g
	for i := 0; i < 3; i++ {
		poll(t, b, a)
	}
	if next.submits != 1 || g.jobs[first].submits != 1 {
		t.Fatal("duplicate job submission")
	}
	// Completion reads the resumed generation's result, never the superseded one.
	next.state = "completed"
	_, _ = b.Poll(context.Background(), a) // fixture result is not a valid proposal
	if len(g.results) == 0 || g.results[0] != first+"_1" {
		t.Fatal("proposal read from the wrong generation", g.results)
	}
}

func TestSteeringSupersedesAWorkerThatAlreadyFinished(t *testing.T) {
	b, a, g := steerFixture(t)
	first := a.Attempt.ID + "_worker"
	poll(t, b, a)
	job := g.jobs[first]
	job.output = `{"type":"thread.started","thread_id":"` + steerSession + `"}` + "\n"
	poll(t, b, a) // drain session output while running
	job.state = "completed"
	message(&a, "msg_late", "worker", "rename the flag")
	poll(t, b, a)
	if r := receipt(t, b, a); r.Live["worker"] == nil || r.Live["worker"].Interrupt != first {
		t.Fatal("finished worker result accepted with undelivered steering")
	}
	poll(t, b, a)
	if job.cancels != 0 || job.state != "completed" {
		t.Fatal("a finished job needs no cancellation and must stay finished")
	}
	if g.jobs[first+"_1"] == nil || len(g.results) != 0 {
		t.Fatal("superseded result was read or resume not started", g.results)
	}
}

func TestLiveSteeringIsBoundedAndVisible(t *testing.T) {
	b, a, g := steerFixture(t)
	first := a.Attempt.ID + "_worker"
	poll(t, b, a)
	g.jobs[first].output = `{"type":"thread.started","thread_id":"` + steerSession + `"}` + "\n"
	poll(t, b, a)
	for i := 1; i <= MaxLiveSteering+1; i++ {
		message(&a, "msg_"+string(rune('a'+i)), "worker", "steer")
		for j := 0; j < 3; j++ {
			poll(t, b, a)
		}
	}
	r := receipt(t, b, a)
	if r.generation("worker") != MaxLiveSteering || len(g.jobs) != MaxLiveSteering+1 {
		t.Fatal("generation bound not enforced", r.generation("worker"), len(g.jobs))
	}
	o := poll(t, b, a)
	if o.Progress == nil || !strings.Contains(o.Progress.Detail, "live steering limit reached") {
		t.Fatal("bound not surfaced", o.Progress)
	}
	if status := a.Revision.MessageStatus(a.Revision.Messages[len(a.Revision.Messages)-1]); !strings.HasPrefix(status, "pending") && !strings.HasPrefix(status, "queued") {
		t.Fatal(status)
	}
	// Past the bound the worker may finish; its supervisor still sees the message.
	g.jobs[r.invocation("worker").ID].state = "completed"
	// The fixture result is not a valid proposal; only the read target matters.
	_, _ = b.Poll(context.Background(), a)
	if len(g.results) != 1 || g.results[0] != r.invocation("worker").ID {
		t.Fatal("bounded worker could not complete", g.results)
	}
}

func TestSupervisorSteeringAndCancellationCoverEveryGeneration(t *testing.T) {
	b, a, g := steerFixture(t)
	r := receipt(t, b, a)
	r.Phase = "supervisor"
	r.Supervisor = &agent.Invocation{ID: a.Attempt.ID + "_supervisor", Role: "supervisor", Directory: "/work/envctl/repos/app", Prompt: "review", Schema: agent.AssessmentSchema(), TimeoutSeconds: 600}
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	sup := a.Attempt.ID + "_supervisor"
	poll(t, b, a)
	g.jobs[sup].output = `{"type":"thread.started","thread_id":"` + steerSession + `"}` + "\n"
	message(&a, "msg_review", "worker", "the worker must keep the API stable")
	poll(t, b, a)
	poll(t, b, a)
	poll(t, b, a)
	resumed := g.jobs[sup+"_1"]
	if resumed == nil || !strings.Contains(resumed.input, "Reject the work if it does not address") || !strings.Contains(resumed.input, "keep the API stable") {
		t.Fatal("supervisor did not receive steering that arrived during review")
	}
	if got := receipt(t, b, a).SupervisorSession; got != steerSession {
		t.Fatal("supervisor session not captured", got)
	}
	if err := b.Cancel(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	if resumed.state != "cancelled" {
		t.Fatal("cancellation missed the live supervisor generation")
	}
}
