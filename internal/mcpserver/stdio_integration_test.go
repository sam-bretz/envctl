package mcpserver

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealCLIStdioReconnectLeavesCoordinatorRunning(t *testing.T) {
	binary := os.Getenv("ENVCTL_MCP_BINARY")
	if binary == "" {
		t.Skip("opt-in compiled CLI stdio acceptance")
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("explicit absolute CLI binary required")
	}
	dir, err := os.MkdirTemp("/tmp", "envctl-mcp-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	done := make(chan error, 1)
	go func() { done <- (&daemon.Server{Store: store}).Serve(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	api := daemon.NewClient(dir)
	for api.Health(ctx) != nil {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	config, err := workflow.Parse([]byte("version: 2\nproject: mcp\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := api.Create(ctx, daemon.CreateRequest{OperationID: "create", Name: "stdio", Task: "Inspect MCP reconnect", Owner: "test", Config: config})
	if err != nil {
		t.Fatal(err)
	}
	// This API test has no execution coordinator, so creating the fixture cannot
	// provision a VM or start any agent.
	for iteration := 0; iteration < 2; iteration++ {
		args := []string{"--state-dir", dir, "mcp", "serve", "--run", run.ID}
		if iteration == 1 {
			args = append(args, "--read-only")
		}
		command := exec.Command(binary, args...)
		var stderr bytes.Buffer
		command.Stderr = &stderr
		client, err := mcp.NewClient(&mcp.Implementation{Name: "stdio-acceptance", Version: "1"}, nil).Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
		if err != nil {
			t.Fatal("real stdio handshake", err, stderr.String())
		}
		var observed workflow.Run
		decode(t, call(t, client, "envctl_run", runInput{Run: run.ID}), "run", &observed)
		if iteration == 0 {
			input := actionInput{Run: run.ID, OperationID: "stdio-message", ExpectedVersion: observed.Version, Revision: observed.CurrentRevision, Action: "message", Recipient: "supervisor", Message: "Keep evidence reviewable"}
			decode(t, call(t, client, "envctl_action", input), "run", &observed)
		} else if len(observed.Current().Messages) != 1 {
			t.Fatal("stdio reconnect lost authoritative mutation")
		}
		if err := client.Close(); err != nil {
			t.Fatal("stdio shutdown", err)
		}
		if err := api.Health(ctx); err != nil {
			t.Fatal("MCP disconnect stopped coordinator", err)
		}
	}
	t.Log("compiled CLI stdio handshake, scoped mutation, disconnect, read-only reconnect and independent coordinator lifetime passed")
}
