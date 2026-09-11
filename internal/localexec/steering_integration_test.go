package localexec

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// A real Claude worker receives a user message while it is running. The
// production Poll state machine records the intent, stops the running job,
// and resumes the same explicit session in a new generation.
func TestRealLiveSteeringResumesRunningClaudeWorker(t *testing.T) {
	if os.Getenv("ENVCTL_STEERING_TEST") != "1" {
		t.Skip("opt-in real harness steering")
	}
	state, runtimeID, credential := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM"), os.Getenv("ENVCTL_HARNESS_CREDENTIAL")
	if state == "" || runtimeID == "" || credential == "" {
		t.Fatal("select the owned acceptance VM and a Claude credential reference explicitly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "envctl-steering-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("retained steering state:", dir)
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := "{kind: claude, credential: \"" + credential + "\"}"
	c, err := workflow.Parse([]byte("version: 2\nproject: steer\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\nagents: {worker: " + h + ", supervisor: " + h + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("steering", "live steering acceptance", "acceptance-test", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.Current().Runtime = workflow.RuntimeState{ID: runtimeID, Provider: "lima", Location: "local", Ready: true, State: "running"}
	a := engine.Assignment{Run: *run, Revision: *run.Current(), Attempt: workflow.Attempt{ID: workflow.ID("attempt"), Node: "code", Number: 1, State: "running", Inputs: map[string]string{}}}
	b := &Backend{Store: store, Provider: vm.NewLima(state)}
	guest := b.guest(a)
	if err = guest.Install(ctx); err != nil {
		t.Fatal(err)
	}
	harness, err := agent.SelectVersion("claude", "", guest)
	if err != nil {
		t.Fatal(err)
	}
	if err = harness.Install(ctx); err != nil {
		t.Fatal(err)
	}
	root := "/work/envctl/steering/" + a.Attempt.ID
	defer func() {
		if err := b.Provider.Exec(context.Background(), runtimeID, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", root}}); err != nil {
			t.Error("fixture cleanup", err)
		}
	}()
	if err = b.Provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "mkdir", "-p", root}}); err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"summary": map[string]any{"type": "string"}, "accepted": map[string]any{"type": "boolean"}}, "required": []string{"summary", "accepted"}, "additionalProperties": false}
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "worker", Cursors: map[string]int64{}, Logs: map[string]string{},
		Worker: agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: root, Schema: schema, TimeoutSeconds: 900,
			Prompt: "This is an envctl live-steering acceptance test. First use Bash to run exactly `sleep 90` in the foreground and wait for it (a deliberate pause, not a problem to solve). After it finishes, write result.txt in the current directory containing exactly ALPHA with no newline, then return accepted=true and a short summary as structured output. Do not start background jobs."}}
	if err = b.save(a, r); err != nil {
		t.Fatal(err)
	}
	first := r.Worker.ID
	var sent bool
	var observed engine.Observation
	for ctx.Err() == nil {
		observed, err = b.Poll(ctx, a)
		if err != nil {
			t.Fatal("retain state after observation error", err)
		}
		r, err = b.load(a)
		if err != nil {
			t.Fatal(err)
		}
		if !sent && r.Session != "" && strings.Contains(r.Logs[first], `"name":"Bash"`) {
			a.Revision.Messages = append(a.Revision.Messages, workflow.Message{ID: workflow.ID("msg"), Node: "code", Recipient: "worker", Body: "Change of plan from the user: write BRAVO instead of ALPHA into result.txt, and include the word BRAVO in your summary. You may stop the pause.", CreatedAt: time.Now()})
			sent = true
			t.Log("sent steering while the worker's Bash pause was running; session", r.Session)
		}
		if r.generation("worker") == 1 && r.Live["worker"].Interrupt == "" {
			s, err := guest.Poll(ctx, r.invocation("worker").ID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if s.State == "completed" {
				break
			}
			if s.State != "running" && s.State != "starting" && s.State != "pending" && s.State != "missing" {
				t.Fatal("resumed generation ended in", s.State)
			}
		}
		select {
		case <-ctx.Done():
		case <-time.After(time.Second):
		}
	}
	if ctx.Err() != nil {
		t.Fatal("steering acceptance expired; state retained")
	}
	resumed := r.invocation("worker")
	original, err := guest.Poll(ctx, first, 0)
	if err != nil || original.State != "cancelled" {
		t.Fatal("original generation was not stopped mid-run", original.State, err)
	}
	if resumed.ID != first+"_1" || resumed.Session != r.Session || harness.Session(r.Logs[resumed.ID]) != r.Session {
		t.Fatal("new generation did not continue the explicit session", resumed.ID)
	}
	delivered := false
	for _, d := range observed.Delivered {
		delivered = delivered || (d.Generation == 1 && d.Role == "worker" && d.Message == a.Revision.Messages[0].ID)
	}
	if !delivered {
		t.Fatal("live delivery not reported to the coordinator", observed.Delivered)
	}
	if observed.Progress == nil || !strings.Contains(strings.Join(observed.Progress.Activity, "\n"), "tool Bash") {
		t.Fatal("progress does not show real harness activity", observed.Progress)
	}
	raw, err := harness.Result(ctx, resumed.ID)
	if err != nil || !strings.Contains(string(raw), "BRAVO") {
		t.Fatal("structured result does not reflect the steering", string(raw), err)
	}
	var file bytes.Buffer
	if err = b.Provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "cat", root + "/result.txt"}, Stdout: &file}); err != nil || file.String() != "BRAVO" {
		t.Fatal("worker output does not reflect the steering", file.String(), err)
	}
	if err = atomicJSON(dir+"/steering-proof.json", map[string]any{"attempt": a.Attempt.ID, "session": r.Session, "generations": r.jobs("worker"), "delivered": observed.Delivered, "activity": observed.Progress.Activity, "result": string(raw)}); err != nil {
		t.Fatal(err)
	}
	t.Log("verified live steering: interrupted generation 0, resumed session", r.Session, "as", resumed.ID, "and the output reflects the message")
}
