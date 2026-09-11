// Package mcpserver exposes coordinator commands to local agent clients. It has
// no workflow store or execution backend: the daemon remains the sole authority.
package mcpserver

import (
	"context"
	"encoding/base64"
	"errors"
	"path/filepath"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type API interface {
	List(context.Context) ([]workflow.Run, error)
	Get(context.Context, string) (*workflow.Run, error)
	Create(context.Context, daemon.CreateRequest) (*workflow.Run, error)
	Action(context.Context, string, daemon.ActionRequest) (*workflow.Run, error)
	Events(context.Context, string, int64) ([]runstore.Event, error)
	Diff(context.Context, string, review.Request) (review.Comparison, error)
	Artifact(context.Context, string) ([]byte, error)
}

type Options struct {
	Version  string
	Run      string // optional fixed run scope; scoped servers cannot create runs
	ReadOnly bool
}

type runInput struct {
	Run string `json:"run" jsonschema:"Run ID from envctl_runs"`
}
type eventsInput struct {
	Run   string `json:"run"`
	After int64  `json:"after,omitempty" jsonschema:"Resume after this durable event sequence; default zero"`
}
type diffInput struct {
	Run      string `json:"run"`
	Revision string `json:"revision,omitempty"`
	Node     string `json:"node"`
	From     string `json:"from,omitempty" jsonschema:"Optional checkpoint ID in the same run; otherwise compare source pins"`
}
type artifactInput struct {
	Run    string `json:"run"`
	Digest string `json:"digest"`
	Limit  int    `json:"limit,omitempty" jsonschema:"Preview byte limit; default 65536, maximum 1048576"`
}
type createInput struct {
	OperationID string `json:"operation_id" jsonschema:"Stable caller-generated ID; reuse exactly the same request after a lost response"`
	Name        string `json:"name"`
	Task        string `json:"task"`
	Owner       string `json:"owner"`
	ConfigYAML  string `json:"config_yaml" jsonschema:"Complete version 2 workflow YAML"`
	Directory   string `json:"directory" jsonschema:"Absolute project directory for resolving configuration paths"`
}
type actionInput struct {
	Run             string              `json:"run"`
	OperationID     string              `json:"operation_id" jsonschema:"Stable caller-generated ID; reuse with identical arguments on reconnect"`
	ExpectedVersion int64               `json:"expected_version" jsonschema:"Observed run version; stale mutations are rejected"`
	Revision        string              `json:"revision" jsonschema:"Observed current revision; never silently updated by the server"`
	Action          string              `json:"action" jsonschema:"One of message, rewind, cancel, priority, plugin-attach, plugin-remove"`
	Node            string              `json:"node,omitempty"`
	Task            string              `json:"task,omitempty"`
	Recipient       string              `json:"recipient,omitempty"`
	Message         string              `json:"message,omitempty"`
	Priority        int                 `json:"priority,omitempty"`
	ConfigYAML      string              `json:"config_yaml,omitempty" jsonschema:"Optional complete amended workflow for rewind; uses the current project directory"`
	Plugin          *workflow.PluginRef `json:"plugin,omitempty"`
	PluginID        string              `json:"plugin_id,omitempty"`
}

func New(api API, options Options) *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "envctl", Version: options.Version}, nil)
	scope := func(run string) error {
		if run == "" {
			return errors.New("run ID is required")
		}
		if options.Run != "" && options.Run != run {
			return errors.New("run is outside this MCP server's scope")
		}
		return nil
	}
	tool := func(name, description string, readonly bool) *mcp.Tool {
		open := !readonly
		return &mcp.Tool{Name: name, Description: description, Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readonly, IdempotentHint: true, OpenWorldHint: &open}}
	}
	mcp.AddTool(server, tool("envctl_runs", "List workflow runs and their current revisions. Inspect a run before any mutation.", true), func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		if options.Run != "" {
			r, err := api.Get(ctx, options.Run)
			return nil, map[string]any{"runs": []*workflow.Run{r}}, err
		}
		runs, err := api.List(ctx)
		return nil, map[string]any{"runs": runs}, err
	})
	mcp.AddTool(server, tool("envctl_run", "Read complete run history, readiness, checkpoints, branches and current version.", true), func(ctx context.Context, _ *mcp.CallToolRequest, in runInput) (*mcp.CallToolResult, any, error) {
		if err := scope(in.Run); err != nil {
			return nil, nil, err
		}
		r, err := api.Get(ctx, in.Run)
		return nil, map[string]any{"run": r}, err
	})
	mcp.AddTool(server, tool("envctl_events", "Read durable workflow events using a resumable sequence cursor.", true), func(ctx context.Context, _ *mcp.CallToolRequest, in eventsInput) (*mcp.CallToolResult, any, error) {
		if err := scope(in.Run); err != nil {
			return nil, nil, err
		}
		if in.After < 0 {
			return nil, nil, errors.New("event cursor cannot be negative")
		}
		events, err := api.Events(ctx, in.Run, in.After)
		return nil, map[string]any{"events": events}, err
	})
	mcp.AddTool(server, tool("envctl_diff", "Compare retained source and checkpoint evidence within a run without changing execution.", true), func(ctx context.Context, _ *mcp.CallToolRequest, in diffInput) (*mcp.CallToolResult, any, error) {
		if err := scope(in.Run); err != nil {
			return nil, nil, err
		}
		comparison, err := api.Diff(ctx, in.Run, review.Request{Revision: in.Revision, Node: in.Node, From: in.From})
		return nil, map[string]any{"comparison": comparison}, err
	})
	mcp.AddTool(server, tool("envctl_artifact", "Read a bounded base64 preview of evidence referenced by the selected run. This does not execute artifact contents.", true), func(ctx context.Context, _ *mcp.CallToolRequest, in artifactInput) (*mcp.CallToolResult, any, error) {
		if err := scope(in.Run); err != nil {
			return nil, nil, err
		}
		if in.Limit == 0 {
			in.Limit = 65536
		}
		if in.Limit < 1 || in.Limit > 1<<20 {
			return nil, nil, errors.New("artifact preview limit must be between 1 and 1048576 bytes")
		}
		r, err := api.Get(ctx, in.Run)
		if err != nil {
			return nil, nil, err
		}
		if !references(r, in.Digest) {
			return nil, nil, errors.New("artifact is not referenced by this run")
		}
		raw, err := api.Artifact(ctx, in.Digest)
		if err != nil {
			return nil, nil, err
		}
		size := len(raw)
		raw = raw[:min(size, in.Limit)]
		return nil, map[string]any{"digest": in.Digest, "size": size, "encoding": "base64", "data": base64.StdEncoding.EncodeToString(raw), "truncated": len(raw) < size}, nil
	})
	if options.ReadOnly {
		return server
	}
	if options.Run == "" {
		mcp.AddTool(server, tool("envctl_create", "Create a workflow invocation from explicit version 2 YAML. May provision a local VM and start agents. Reuse operation_id and identical input to recover a lost response.", false), func(ctx context.Context, _ *mcp.CallToolRequest, in createInput) (*mcp.CallToolResult, any, error) {
			if in.OperationID == "" {
				return nil, nil, errors.New("operation_id is required")
			}
			if !filepath.IsAbs(in.Directory) {
				return nil, nil, errors.New("project directory must be absolute")
			}
			config, err := parseConfig(in.ConfigYAML, in.Directory)
			if err != nil {
				return nil, nil, err
			}
			r, err := api.Create(ctx, daemon.CreateRequest{OperationID: in.OperationID, Name: in.Name, Task: in.Task, Owner: in.Owner, Config: config})
			return nil, map[string]any{"run": r}, err
		})
	}
	mcp.AddTool(server, tool("envctl_action", "Send a message, rewind, cancel, change priority, or amend invocation plugins. Requires exact observed version/revision and a stable operation_id. Rewind/plugin changes create revisions. Human approval and publication are not agent tools.", false), func(ctx context.Context, _ *mcp.CallToolRequest, in actionInput) (*mcp.CallToolResult, any, error) {
		if err := scope(in.Run); err != nil {
			return nil, nil, err
		}
		if !slices.Contains([]string{"message", "rewind", "cancel", "priority", "plugin-attach", "plugin-remove"}, in.Action) {
			return nil, nil, errors.New("unsupported agent action; human approval and checkpoint publication are separate operations")
		}
		if in.OperationID == "" || in.ExpectedVersion < 1 || in.Revision == "" {
			return nil, nil, errors.New("operation_id, positive expected_version and revision are required")
		}
		req := daemon.ActionRequest{OperationID: in.OperationID, ExpectedVersion: in.ExpectedVersion, Revision: in.Revision, Action: in.Action, Node: in.Node, Task: in.Task, Recipient: in.Recipient, Message: in.Message, Priority: in.Priority, Plugin: in.Plugin, PluginID: in.PluginID}
		if in.ConfigYAML != "" {
			if in.Action != "rewind" {
				return nil, nil, errors.New("config_yaml is only valid for rewind")
			}
			r, err := api.Get(ctx, in.Run)
			if err != nil {
				return nil, nil, err
			}
			// Parsing needs only the directory. The mutation still carries the caller's
			// version/revision and the daemon rejects any concurrent change.
			revision := r.Revision(in.Revision)
			if revision == nil {
				return nil, nil, errors.New("requested revision does not exist")
			}
			config, err := parseConfig(in.ConfigYAML, revision.Config.Dir)
			if err != nil {
				return nil, nil, err
			}
			req.Config = &config
		}
		r, err := api.Action(ctx, in.Run, req)
		return nil, map[string]any{"run": r}, err
	})
	return server
}

func parseConfig(raw, directory string) (workflow.Config, error) {
	if len(raw) > 2<<20 {
		return workflow.Config{}, errors.New("workflow configuration exceeds 2 MiB")
	}
	config, err := workflow.Parse([]byte(raw))
	if err != nil {
		return workflow.Config{}, err
	}
	config.Dir = directory
	return config, nil
}

func references(run *workflow.Run, digest string) bool {
	if digest == "" {
		return false
	}
	resultHas := func(result *workflow.Result) bool {
		if result == nil {
			return false
		}
		if result.Review.EvidenceDigest == digest {
			return true
		}
		for _, a := range result.Artifacts {
			if a.Digest == digest {
				return true
			}
		}
		for _, a := range result.Sources {
			if a.Digest == digest {
				return true
			}
		}
		for _, a := range result.SourceObjects {
			if a.Digest == digest {
				return true
			}
		}
		for _, c := range result.Checks {
			if c.EvidenceDigest == digest {
				return true
			}
		}
		for _, d := range result.Datasets {
			if d.Source.Digest == digest || d.Evidence.Digest == digest {
				return true
			}
		}
		return false
	}
	for _, revision := range run.Revisions {
		for _, child := range revision.ChildRuntimes {
			if child == nil {
				continue
			}
			if child.Recovery != nil && child.Recovery.EvidenceDigest == digest {
				return true
			}
			for _, probe := range child.Readiness {
				if probe.EvidenceDigest == digest {
					return true
				}
			}
		}
		if revision.Recovery != nil && revision.Recovery.EvidenceDigest == digest {
			return true
		}
		for _, probe := range revision.Readiness {
			if probe.EvidenceDigest == digest {
				return true
			}
		}
		for _, cp := range revision.Checkpoints {
			if resultHas(&cp.Result) {
				return true
			}
		}
		for _, attempt := range revision.Attempts {
			if resultHas(attempt.Result) {
				return true
			}
		}
	}
	return false
}
