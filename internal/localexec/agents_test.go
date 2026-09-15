package localexec

import (
	"testing"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func containsFlag(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag {
			return i+1 < len(args) && args[i+1] == value
		}
	}
	return false
}

func containsFlagName(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

// TestWorkerAndSupervisorInvocationsUseTheResolvedModel proves that the
// job submitted for each role carries the model Config.NodeAgents resolves
// for that node: a node override for the worker, and the inherited
// run-wide model for the supervisor (no node override for that role).
func TestWorkerAndSupervisorInvocationsUseTheResolvedModel(t *testing.T) {
	b, a := fixture(t)
	t.Setenv("ENVCTL_AGENTS_FIXTURE_KEY", "fixture-secret-key")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_AGENTS_FIXTURE_KEY"
	a.Revision.Config.Agents.Supervisor.Credential = "env:ENVCTL_AGENTS_FIXTURE_KEY"
	a.Revision.Config.Agents.Supervisor.Model = "run-wide-supervisor-model"
	code := a.Revision.Config.Workflow.Nodes["code"]
	code.Agents.Worker.Model = "node-worker-model"
	a.Revision.Config.Workflow.Nodes["code"] = code
	g := &steerGuest{jobs: map[string]*fakeJob{}}
	b.Provider = g
	resolved := a.Revision.Config.NodeAgents(a.Attempt.Node)
	if resolved.Worker.Model != "node-worker-model" || resolved.Supervisor.Model != "run-wide-supervisor-model" {
		t.Fatalf("resolver did not resolve as expected: %+v", resolved)
	}
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "worker", Cursors: map[string]int64{}, Logs: map[string]string{},
		Worker: agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: "/work/envctl/repos/app", Prompt: "implement", Schema: map[string]any{"type": "object"}, Model: resolved.Worker.Model, TimeoutSeconds: 600}}
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	poll(t, b, a)
	job := g.jobs[a.Attempt.ID+"_worker"]
	if job == nil || !containsFlag(job.args, "--model", "node-worker-model") {
		t.Fatalf("worker job did not receive the node-resolved model: %+v", job)
	}

	r.Phase = "supervisor"
	r.Supervisor = &agent.Invocation{ID: a.Attempt.ID + "_supervisor", Role: "supervisor", Directory: "/work/envctl/repos/app", Prompt: "review", Schema: agent.AssessmentSchema(), Model: resolved.Supervisor.Model, TimeoutSeconds: 600}
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	poll(t, b, a)
	sup := g.jobs[a.Attempt.ID+"_supervisor"]
	if sup == nil || !containsFlag(sup.args, "--model", "run-wide-supervisor-model") {
		t.Fatalf("supervisor job did not receive the run-wide model: %+v", sup)
	}
}

// TestInvocationsOmitModelFlagWhenUnset proves that a role with neither a
// node override nor a run-wide model omits --model, leaving the harness
// default in effect.
func TestInvocationsOmitModelFlagWhenUnset(t *testing.T) {
	b, a := fixture(t)
	t.Setenv("ENVCTL_AGENTS_FIXTURE_KEY2", "fixture-secret-key")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_AGENTS_FIXTURE_KEY2"
	g := &steerGuest{jobs: map[string]*fakeJob{}}
	b.Provider = g
	resolved := a.Revision.Config.NodeAgents(a.Attempt.Node)
	if resolved.Worker.Model != "" {
		t.Fatal("fixture must start with no configured model")
	}
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "worker", Cursors: map[string]int64{}, Logs: map[string]string{},
		Worker: agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: "/work/envctl/repos/app", Prompt: "implement", Schema: map[string]any{"type": "object"}, Model: resolved.Worker.Model, TimeoutSeconds: 600}}
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	poll(t, b, a)
	job := g.jobs[a.Attempt.ID+"_worker"]
	if job == nil || containsFlagName(job.args, "--model") {
		t.Fatalf("harness default expected but --model was present: %+v", job)
	}
}

// TestSteeringResumeKeepsGenerationZeroModel proves that a live-steering
// resume carries forward the model the interrupted generation ran with,
// even though the resume never re-resolves Config.NodeAgents.
func TestSteeringResumeKeepsGenerationZeroModel(t *testing.T) {
	b, a := fixture(t)
	t.Setenv("ENVCTL_STEER_MODEL_KEY", "fixture-secret-key")
	a.Revision.Config.Agents.Worker.Credential = "env:ENVCTL_STEER_MODEL_KEY"
	a.Revision.Config.Agents.Supervisor.Credential = "env:ENVCTL_STEER_MODEL_KEY"
	a.Attempt.Number, a.Attempt.State = 1, "running"
	g := &steerGuest{jobs: map[string]*fakeJob{}}
	b.Provider = g
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "worker", Cursors: map[string]int64{}, Logs: map[string]string{},
		Worker: agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: "/work/envctl/repos/app", Prompt: "implement the task", Schema: map[string]any{"type": "object"}, Model: "generation-zero-model", TimeoutSeconds: 600}}
	r.include(a, "worker", func(m workflow.Message) bool { return m.Recipient == "worker" })
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	first := a.Attempt.ID + "_worker"
	poll(t, b, a) // start generation 0
	message(&a, "msg_live", "worker", "keep the API stable")
	poll(t, b, a) // session unknown yet: nothing steered
	g.jobs[first].output = `{"type":"thread.started","thread_id":"` + steerSession + `"}` + "\n"
	poll(t, b, a) // records the live steering intent
	poll(t, b, a) // cancels generation 0
	poll(t, b, a) // starts the resumed generation
	resumed := g.jobs[first+"_1"]
	if resumed == nil {
		t.Fatal("steering did not start a resumed generation")
	}
	if !containsFlag(resumed.args, "--model", "generation-zero-model") {
		t.Fatalf("resumed generation lost its generation-0 model: %+v", resumed.args)
	}
}
