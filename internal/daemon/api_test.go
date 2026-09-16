package daemon

import (
	"context"
	"errors"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type coordinatorFunc func(context.Context) error

func (f coordinatorFunc) Run(ctx context.Context) error { return f(ctx) }

func TestCoordinatorFailureStopsOwningServer(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "envctl-owner-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	s, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	failure := errors.New("execution coordinator stopped")
	server := &Server{Store: s, Coordinator: coordinatorFunc(func(context.Context) error { return failure })}
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), failure.Error()) {
			t.Fatal("coordinator failure was lost", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("API continued running without its coordinator")
	}
}

func testAPI(t *testing.T) (*Client, *runstore.Store) {
	t.Helper()
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := httptest.NewServer((&Server{Store: store}).Handler())
	t.Cleanup(server.Close)
	return &Client{HTTP: server.Client(), BaseURL: server.URL}, store
}
func create(t *testing.T, c *Client) *workflow.Run {
	t.Helper()
	config, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Create(context.Background(), CreateRequest{OperationID: "create", Task: "Build exports", Name: "Exports", Owner: "test", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func TestClientsShareDurableRevisionCheckedAPI(t *testing.T) {
	c, _ := testAPI(t)
	ctx := context.Background()
	r := create(t, c)
	if err := c.Health(ctx); err != nil {
		t.Fatal(err)
	}
	runs, err := c.List(ctx)
	if err != nil || len(runs) != 1 {
		t.Fatal(err)
	}
	req := ActionRequest{OperationID: "msg", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "message", Recipient: "supervisor", Message: "Use the existing API"}
	updated, err := c.Action(ctx, r.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Current().Messages) != 1 {
		t.Fatal("message not persisted")
	}
	req.OperationID = "second"
	if _, err = c.Action(ctx, r.ID, req); !errors.Is(err, workflow.ErrConflict) {
		t.Fatalf("stale client accepted: %v", err)
	}
	events, err := c.Events(ctx, r.ID, 0)
	if err != nil || len(events) != 2 {
		t.Fatal("event history", err)
	}
	tail, err := c.Events(ctx, r.ID, events[0].Sequence)
	if err != nil || len(tail) != 1 {
		t.Fatal("cursor", err)
	}
}

func TestCloseReleasesNoEvidenceAndCancelLeavesCompletedWorkAlone(t *testing.T) {
	c, store := testAPI(t)
	r := create(t, c)
	artifact, err := store.PutArtifact("result", "text/plain", []byte("retained evidence"))
	if err != nil {
		t.Fatal(err)
	}
	checkpoint := workflow.Checkpoint{ID: "cp_done", Revision: r.CurrentRevision, Node: "approved-change", Result: workflow.Result{
		Artifacts: []workflow.Artifact{artifact},
		PRs:       map[string]string{"app": "https://github.com/example/app/pull/7"},
	}}
	updated, err := store.Mutate(context.Background(), r.ID, r.Version, "finish", "fixture.completed", nil, func(run *workflow.Run) error {
		run.Current().State = "completed"
		run.Current().Runtime = workflow.RuntimeState{ID: "vm-done", Ready: true, State: "running"}
		run.Current().Checkpoints[checkpoint.Node] = checkpoint
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = c.Action(context.Background(), r.ID, ActionRequest{OperationID: "cancel-completed", ExpectedVersion: updated.Version, Revision: updated.CurrentRevision, Action: "cancel"})
	if err == nil || !strings.Contains(err.Error(), "close") {
		t.Fatalf("cancel did not reject completed work with close guidance: %v", err)
	}
	untouched, err := c.Get(context.Background(), r.ID)
	if err != nil || untouched.Current().State != "completed" {
		t.Fatalf("cancel changed completed work: %v %+v", err, untouched)
	}

	closed, err := c.Action(context.Background(), r.ID, ActionRequest{OperationID: "close-completed", ExpectedVersion: untouched.Version, Revision: untouched.CurrentRevision, Action: "close"})
	if err != nil {
		t.Fatal(err)
	}
	if closed.Current().State != "completed" || closed.ClosedAt.IsZero() || closed.Current().Checkpoints[checkpoint.Node].Result.Artifacts[0].Digest != artifact.Digest {
		t.Fatalf("close lost completed evidence: %+v", closed)
	}
	if got, err := c.Artifact(context.Background(), artifact.Digest); err != nil || string(got) != "retained evidence" {
		t.Fatalf("closed artifact unavailable: %v %q", err, got)
	}
	if prs := closed.PullRequests(); len(prs) != 1 || prs[0].URL != checkpoint.Result.PRs["app"] {
		t.Fatalf("closed PR evidence unavailable: %+v", prs)
	}
}
func TestAPIHasNoAgentCompletionBypass(t *testing.T) {
	c, _ := testAPI(t)
	r := create(t, c)
	_, err := c.Action(context.Background(), r.ID, ActionRequest{OperationID: "bypass", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "checkpoint"})
	if err == nil {
		t.Fatal("agent could checkpoint through control API")
	}
}
func TestArtifactAPI(t *testing.T) {
	c, s := testAPI(t)
	a, err := s.PutArtifact("plan", "text/plain", []byte("plan evidence"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Artifact(context.Background(), a.Digest)
	if err != nil || string(b) != "plan evidence" {
		t.Fatal(err)
	}
}
func TestServeSingleOwnerAndRestart(t *testing.T) {
	// Use a short path: Darwin Unix sockets have a 104-byte pathname limit.
	dir, err := os.MkdirTemp("/tmp", "envctl-daemon-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	server := &Server{Store: s}
	go func() { done <- server.Serve(ctx) }()
	client := NewClient(s.Dir)
	deadline := time.Now().Add(3 * time.Second)
	for client.Health(context.Background()) != nil {
		select {
		case err := <-done:
			t.Fatal(err)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := server.Serve(context.Background()); err == nil {
		t.Fatal("second coordinator acquired ownership")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("coordinator failed to stop")
	}
}

func TestAskingRecordsAQuestionWithoutTouchingTheWork(t *testing.T) {
	c, store := testAPI(t)
	r := create(t, c)
	updated, err := store.Mutate(context.Background(), r.ID, r.Version, "start", "fixture.attempt", nil, func(run *workflow.Run) error {
		run.Current().Attempts = []workflow.Attempt{{ID: "attempt_1", Node: "code", Number: 1, State: "running"}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	asked, err := c.Action(context.Background(), r.ID, ActionRequest{OperationID: "ask-1", ExpectedVersion: updated.Version, Revision: updated.CurrentRevision, Action: "ask", Node: "code", Message: "is there a PR open for this?", Actor: "sam"})
	if err != nil {
		t.Fatal(err)
	}
	qs := asked.Current().Questions
	if len(qs) != 1 || qs[0].Attempt != "attempt_1" || qs[0].Asker != "sam" || qs[0].State != workflow.QuestionPending {
		t.Fatalf("question not recorded: %+v", qs)
	}
	if asked.Current().Attempts[0].State != "running" || len(asked.Current().Messages) != 0 {
		t.Fatal("asking steered or changed the running attempt")
	}
	if _, err = c.Action(context.Background(), r.ID, ActionRequest{OperationID: "ask-2", ExpectedVersion: asked.Version, Revision: asked.CurrentRevision, Action: "ask", Node: "qa", Message: "why?"}); err == nil {
		t.Fatal("accepted a question for a stage that has not started")
	}
}

func TestOnlySteeringAskingAndChoosingMayAddressAVariation(t *testing.T) {
	c, store := testAPI(t)
	r := create(t, c)
	branched, err := store.Mutate(context.Background(), r.ID, r.Version, "branch", "fixture.branch", nil, func(run *workflow.Run) error {
		rev := run.Current()
		rev.State = "active"
		nodes := rev.Config.Workflow.Nodes
		design := nodes["design"]
		design.Variations = 3
		nodes["design"] = design
		for _, node := range []string{"task", "plan", "design"} {
			rev.Checkpoints[node] = workflow.Checkpoint{ID: "cp_" + node, Node: node}
		}
		if err := run.Branch(run.CurrentRevision, "design", []workflow.Variation{{Name: "polling", Rationale: "r"}, {Name: "streaming", Rationale: "r"}}, time.Now()); err != nil {
			return err
		}
		// Every candidate has finished its stages, so one can be chosen.
		for i := range run.Revisions {
			v := &run.Revisions[i]
			v.State = "active"
			for id, n := range v.Config.Workflow.Nodes {
				if n.Kind != "change" {
					v.Checkpoints[id] = workflow.Checkpoint{ID: "cp_" + id + v.ID, Node: id}
				}
			}
			v.Attempts = []workflow.Attempt{{ID: "att_" + v.ID, Node: "design", State: "checkpointed"}}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sibling := branched.Revisions[1].ID
	version := branched.Version
	act := func(req ActionRequest) (*workflow.Run, error) {
		req.OperationID, req.ExpectedVersion = workflow.ID("op"), version
		run, err := c.Action(context.Background(), r.ID, req)
		if err == nil {
			version = run.Version
		}
		return run, err
	}

	if _, err := act(ActionRequest{Revision: sibling, Action: "message", Node: "design", Recipient: "worker", Message: "prefer the standard library"}); err != nil {
		t.Fatalf("could not steer a variation: %v", err)
	}
	if _, err := act(ActionRequest{Revision: sibling, Action: "ask", Node: "design", Message: "why polling?"}); err != nil {
		t.Fatalf("could not ask a variation: %v", err)
	}
	// Approval and rewind still require the current revision: a variation
	// must not be approved or rewound from under the comparison.
	for _, action := range []string{"approve", "rewind"} {
		if _, err := act(ActionRequest{Revision: sibling, Action: action, Node: "design", Attempt: "att_" + sibling}); err == nil {
			t.Fatalf("%s was accepted on a variation that is not current", action)
		}
	}

	chosen, err := act(ActionRequest{Revision: sibling, Action: "choose"})
	if err != nil {
		t.Fatal(err)
	}
	if chosen.CurrentRevision != sibling || !chosen.Revision(sibling).Variant.Chosen {
		t.Fatal("choosing did not make the variation current")
	}
	if chosen.Revisions[0].State != workflow.NotChosen || chosen.Revisions[2].State != workflow.NotChosen {
		t.Fatal("the other variations were not retired")
	}
}
