package mcpserver

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func fixture(t *testing.T) (*daemon.Client, *runstore.Store) {
	t.Helper()
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	server := httptest.NewServer((&daemon.Server{Store: store}).Handler())
	t.Cleanup(server.Close)
	return &daemon.Client{HTTP: server.Client(), BaseURL: server.URL}, store
}
func session(t *testing.T, api API, options Options) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	left, right := mcp.NewInMemoryTransports()
	server, err := New(api, options).Connect(ctx, left, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	client, err := mcp.NewClient(&mcp.Implementation{Name: "acceptance", Version: "1"}, nil).Connect(ctx, right, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { client.Close() })
	return client
}
func call(t *testing.T, s *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	result, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(name, err)
	}
	return result
}
func decode(t *testing.T, r *mcp.CallToolResult, key string, out any) {
	t.Helper()
	if r.IsError {
		t.Fatal(r.Content)
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]json.RawMessage
	if err = json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(envelope[key], out); err != nil {
		t.Fatal(err, string(raw))
	}
}
func create(t *testing.T, s *mcp.ClientSession, operation string) *workflow.Run {
	t.Helper()
	result := call(t, s, "envctl_create", createInput{OperationID: operation, Name: "MCP feature", Task: "Build a feature", Owner: "test", Directory: t.TempDir(), ConfigYAML: "version: 2\nproject: mcp\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"})
	var r workflow.Run
	decode(t, result, "run", &r)
	return &r
}

func TestMCPClientsShareVersionFencedReplayableCommands(t *testing.T) {
	api, store := fixture(t)
	first := session(t, api, Options{Version: "test"})
	tools, err := first.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 7 {
		t.Fatalf("tool surface = %d", len(tools.Tools))
	}
	r := create(t, first, "create")
	second := session(t, api, Options{Version: "test"})
	input := actionInput{Run: r.ID, OperationID: "message", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "message", Recipient: "supervisor", Message: "Keep scope narrow"}
	var updated workflow.Run
	decode(t, call(t, first, "envctl_action", input), "run", &updated)
	var replay workflow.Run
	decode(t, call(t, second, "envctl_action", input), "run", &replay)
	if replay.Version != updated.Version || len(replay.Current().Messages) != 1 {
		t.Fatal("reconnect duplicated mutation")
	}
	input.OperationID = "stale"
	if !call(t, second, "envctl_action", input).IsError {
		t.Fatal("stale client mutation accepted")
	}
	input.ExpectedVersion = updated.Version
	input.OperationID = "approval"
	input.Action = "approve"
	if !call(t, first, "envctl_action", input).IsError {
		t.Fatal("MCP client bypassed human approval")
	}
	input.Action = "rewind"
	input.OperationID = "rewind"
	input.Node = "plan"
	decode(t, call(t, second, "envctl_action", input), "run", &updated)
	if len(updated.Revisions) != 2 || updated.CurrentRevision == r.CurrentRevision {
		t.Fatal("rewind did not create revision")
	}
	decode(t, call(t, first, "envctl_action", input), "run", &replay)
	if len(replay.Revisions) != 2 {
		t.Fatal("rewind replay duplicated revision")
	}
	input.OperationID = "old-revision"
	input.ExpectedVersion = updated.Version
	if !call(t, first, "envctl_action", input).IsError {
		t.Fatal("old revision mutation admitted")
	}
	var events []runstore.Event
	decode(t, call(t, first, "envctl_events", eventsInput{Run: r.ID}), "events", &events)
	if len(events) < 3 {
		t.Fatal("durable event history missing")
	}
	var later []runstore.Event
	decode(t, call(t, second, "envctl_events", eventsInput{Run: r.ID, After: events[len(events)-1].Sequence}), "events", &later)
	if len(later) != 0 {
		t.Fatal("event cursor repeated history")
	}
	stored, err := store.Get(context.Background(), r.ID)
	if err != nil || stored.Version != updated.Version {
		t.Fatal("MCP state diverged from authoritative store", err)
	}
}

func TestMCPRunScopeReadOnlyAndArtifactReferenceChecks(t *testing.T) {
	api, store := fixture(t)
	all := session(t, api, Options{Version: "test"})
	a, b := create(t, all, "a"), create(t, all, "b")
	artifact, err := store.PutArtifact("evidence", "text/plain", []byte("scoped evidence"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Mutate(context.Background(), a.ID, a.Version, "evidence", "fixture", nil, func(r *workflow.Run) error {
		r.Current().Recovery = &workflow.Recovery{Phase: "fixture", Detail: "retained evidence", EvidenceDigest: artifact.Digest}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	scoped := session(t, api, Options{Version: "test", Run: a.ID, ReadOnly: true})
	tools, err := scoped.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 5 {
		t.Fatal("read-only server exposes mutations")
	}
	for _, tool := range tools.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatal("read-only hint missing")
		}
	}
	var runs []workflow.Run
	decode(t, call(t, scoped, "envctl_runs", struct{}{}), "runs", &runs)
	if len(runs) != 1 || runs[0].ID != a.ID {
		t.Fatal("run scope leaked registry")
	}
	if !call(t, scoped, "envctl_run", runInput{Run: b.ID}).IsError {
		t.Fatal("cross-run read allowed")
	}
	if !call(t, scoped, "envctl_artifact", artifactInput{Run: b.ID, Digest: artifact.Digest}).IsError {
		t.Fatal("cross-run artifact allowed")
	}
	if !call(t, all, "envctl_artifact", artifactInput{Run: b.ID, Digest: artifact.Digest}).IsError {
		t.Fatal("artifact ownership was only enforced by server scope")
	}
	result := call(t, scoped, "envctl_artifact", artifactInput{Run: a.ID, Digest: artifact.Digest, Limit: 6})
	var encoded string
	decode(t, result, "data", &encoded)
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(raw) != "scoped" {
		t.Fatal("bounded artifact content", err)
	}
	var truncated bool
	decode(t, result, "truncated", &truncated)
	if !truncated {
		t.Fatal("truncation not reported")
	}
	if _, err := scoped.CallTool(context.Background(), &mcp.CallToolParams{Name: "envctl_action", Arguments: map[string]any{}}); err == nil {
		t.Fatal("read-only mutation tool is callable")
	}
}

func TestMCPRejectsInvalidInputsBeforeDaemonMutation(t *testing.T) {
	api, store := fixture(t)
	s := session(t, api, Options{Version: "test"})
	r := create(t, s, "create")
	for _, args := range []any{
		map[string]any{"run": r.ID, "action": "message"},
		actionInput{Run: r.ID, Action: "cancel", Revision: r.CurrentRevision, ExpectedVersion: r.Version},
		actionInput{Run: r.ID, Action: "publish", OperationID: "publish", Revision: r.CurrentRevision, ExpectedVersion: r.Version},
		actionInput{Run: r.ID, Action: "message", OperationID: "config", Revision: r.CurrentRevision, ExpectedVersion: r.Version, ConfigYAML: "version: 2"},
	} {
		result, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "envctl_action", Arguments: args})
		if err == nil && !result.IsError {
			t.Fatal("invalid mutation accepted")
		}
	}
	current, err := store.Get(context.Background(), r.ID)
	if err != nil || current.Version != r.Version {
		t.Fatal("invalid MCP call changed state", err)
	}
}

func TestMCPPluginAmendmentAndConfigReplayPreserveOriginalInputs(t *testing.T) {
	api, _ := fixture(t)
	s := session(t, api, Options{Version: "test"})
	r := create(t, s, "create")
	root := t.TempDir()
	descriptor := plugin.Descriptor{ID: "reference", Version: "1.0.0", Protocol: 1, Provides: []string{"arithmetic.fixture"}, Operations: []string{"probe"}, Command: []string{"python3", "main.py"}}
	raw, _ := json.Marshal(descriptor)
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	input := actionInput{Run: r.ID, OperationID: "attach", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "plugin-attach", Plugin: &workflow.PluginRef{ID: "reference", Version: "1.0.0", Source: root}}
	var attached workflow.Run
	decode(t, call(t, s, "envctl_action", input), "run", &attached)
	if len(attached.Current().Config.Plugins) != 1 || attached.Current().Config.Plugins[0].Digest == "" || r.CurrentRevision == attached.CurrentRevision {
		t.Fatal("MCP attachment did not create a frozen invocation revision")
	}
	input = actionInput{Run: r.ID, OperationID: "config-rewind", ExpectedVersion: attached.Version, Revision: attached.CurrentRevision, Action: "rewind", Node: "plan", ConfigYAML: "version: 2\nproject: mcp\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"}
	var amended workflow.Run
	decode(t, call(t, s, "envctl_action", input), "run", &amended)
	changed := workflow.Clone(amended.Current().Config)
	changed.Dir = t.TempDir()
	_, err := api.Action(context.Background(), r.ID, daemon.ActionRequest{OperationID: "later-change", ExpectedVersion: amended.Version, Revision: amended.CurrentRevision, Action: "rewind", Node: "plan", Config: &changed})
	if err != nil {
		t.Fatal(err)
	}
	// The MCP process has no local replay state. It reconstructs config paths
	// from the originally requested revision, not the latest changed directory.
	var replay workflow.Run
	decode(t, call(t, session(t, api, Options{Version: "test"}), "envctl_action", input), "run", &replay)
	if workflow.Digest(replay) != workflow.Digest(amended) {
		t.Fatal("config amendment lost replay identity after another revision")
	}
}
