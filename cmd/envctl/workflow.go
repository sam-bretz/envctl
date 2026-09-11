package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sam-bretz/envctl/internal/checkpoint"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/feature"
	"github.com/sam-bretz/envctl/internal/localexec"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/tui"
	"github.com/sam-bretz/envctl/internal/workflow"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

func stateDir(g *globals) (string, error) {
	if g.stateDir != "" {
		return filepath.Abs(g.stateDir)
	}
	return runstore.DefaultDir()
}
func workflowRoot(ctx context.Context, dir string) string {
	if root, err := feature.Toplevel(ctx, dir); err == nil {
		return root
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	return abs
}
func connect(ctx context.Context, g *globals) (*daemon.Client, error) {
	dir, err := stateDir(g)
	if err != nil {
		return nil, err
	}
	client := daemon.NewClient(dir)
	if client.Health(ctx) == nil {
		return client, nil
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	log, err := os.OpenFile(filepath.Join(dir, "daemon.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer log.Close()
	child := exec.Command(exe, "--state-dir", dir, "daemon", "serve")
	child.Stdin = nil
	child.Stdout = log
	child.Stderr = log
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = child.Start(); err != nil {
		return nil, err
	}
	_ = child.Process.Release()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	for {
		if client.Health(ctx) == nil {
			return client, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-timeout.C:
			return nil, fmt.Errorf("daemon did not start; inspect %s", filepath.Join(dir, "daemon.log"))
		case <-tick.C:
		}
	}
}
func launchTUI(cmd *cobra.Command, g *globals) error {
	if g.jsonOut || !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
		return cmd.Help()
	}
	client, err := connect(cmd.Context(), g)
	if err != nil {
		return err
	}
	return tui.Run(cmd.Context(), client, workflowRoot(cmd.Context(), g.dir))
}
func tuiCmd(g *globals) *cobra.Command {
	return &cobra.Command{Use: "ui", Short: "Attach the Bubble Tea workflow dashboard", RunE: func(cmd *cobra.Command, args []string) error { return launchTUI(cmd, g) }}
}
func daemonCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "daemon", Short: "Manage the persistent workflow coordinator"}
	c.AddCommand(&cobra.Command{Use: "serve", Short: "Run the coordinator in the foreground", RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := stateDir(g)
		if err != nil {
			return err
		}
		store, err := runstore.Open(dir)
		if err != nil {
			return err
		}
		defer store.Close()
		coordinator := &engine.Engine{Store: store, Backend: localexec.New(store), OnError: func(err error) { fmt.Fprintln(cmd.ErrOrStderr(), "workflow reconciliation:", err) }}
		return (&daemon.Server{Store: store, Coordinator: coordinator}).Serve(cmd.Context())
	}})
	c.AddCommand(&cobra.Command{Use: "status", Short: "Check coordinator availability", RunE: func(cmd *cobra.Command, args []string) error {
		dir, err := stateDir(g)
		if err != nil {
			return err
		}
		if err = daemon.NewClient(dir).Health(cmd.Context()); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"running": true, "socket": daemon.Socket(dir)})
	}})
	return c
}
func runCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "run", Short: "Create, inspect, and steer checkpointed workflows"}
	fixture := &cobra.Command{Use: "fixture", Short: "Scaffold reproducible test data services"}
	fixture.AddCommand(&cobra.Command{Use: "init <directory>", Short: "Create an HTTP record emulator, Compose fragment and seed file", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		root := workflowRoot(cmd.Context(), g.dir)
		destination := args[0]
		if !filepath.IsAbs(destination) {
			destination = filepath.Join(root, destination)
		}
		destination, err := filepath.Abs(destination)
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(root, destination)
		if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("fixture directory must be inside the workflow repository")
		}
		if err = checkpoint.ScaffoldFixture(destination, relative); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]string{"directory": destination, "compose": filepath.Join(relative, "compose.yaml"), "seed": filepath.Join(relative, "seed.json"), "adapter": "http-fixture"})
	}})
	c.AddCommand(fixture)
	c.AddCommand(runArtifactCmd(g))
	c.AddCommand(runDiffCmd(g))
	var task, name, taskRef, op string
	var pluginFiles []string
	create := &cobra.Command{Use: "create", Short: "Create a workflow invocation from this repository", RunE: func(cmd *cobra.Command, args []string) error {
		config, err := workflow.Load(workflowRoot(cmd.Context(), g.dir))
		if err != nil {
			return err
		}
		for _, file := range pluginFiles {
			ref, err := plugin.ReadRef(file)
			if err != nil {
				return err
			}
			config.Plugins = append(config.Plugins, ref)
		}
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		if name == "" {
			name = task
		}
		if op == "" {
			op = workflow.ID("op")
		}
		owner := os.Getenv("USER")
		run, err := client.Create(cmd.Context(), daemon.CreateRequest{OperationID: op, Name: name, Task: task, TaskRef: taskRef, Owner: owner, Config: config})
		if err != nil {
			return err
		}
		return printRun(cmd, g, run)
	}}
	create.Flags().StringVar(&task, "task", "", "objective to complete")
	_ = create.MarkFlagRequired("task")
	create.Flags().StringVar(&name, "name", "", "display name (defaults to objective)")
	create.Flags().StringVar(&taskRef, "task-ref", "", "issue or story URL")
	create.Flags().StringVar(&op, "operation-id", "", "idempotency key for retried creation")
	create.Flags().StringArrayVar(&pluginFiles, "plugin-file", nil, "invocation plugin reference YAML (repeatable)")
	c.AddCommand(create)
	c.AddCommand(&cobra.Command{Use: "validate", Short: "Validate workflow configuration and DAG without starting a runtime", RunE: func(cmd *cobra.Command, args []string) error {
		config, err := workflow.Load(workflowRoot(cmd.Context(), g.dir))
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(config)
	}})
	c.AddCommand(&cobra.Command{Use: "list", Short: "List workflow runs", RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		runs, err := client.List(cmd.Context())
		if err != nil {
			return err
		}
		if g.jsonOut {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(runs)
		}
		for i := range runs {
			if err = printRun(cmd, g, &runs[i]); err != nil {
				return err
			}
		}
		return nil
	}})
	for _, kind := range []string{"show", "readiness", "checkpoints"} {
		c.AddCommand(&cobra.Command{Use: kind + " <run>", Short: "Inspect workflow " + kind, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
			client, err := connect(cmd.Context(), g)
			if err != nil {
				return err
			}
			run, err := client.Get(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			switch kind {
			case "readiness":
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"revision": run.CurrentRevision, "required": run.Current().Requirements(), "discovered": run.Current().DiscoveredRequirements, "probes": run.Current().Readiness, "children": run.Current().ChildRuntimes, "limits": nodeLimits(run.Current()), "unresolved": run.Current().ReadinessProblems(time.Now(), run.Current().Requirements())})
			case "checkpoints":
				return json.NewEncoder(cmd.OutOrStdout()).Encode(run.Current().Checkpoints)
			}
			return printRun(cmd, g, run)
		}})
	}
	for _, kind := range []string{"message", "rewind", "cancel", "approve", "priority"} {
		c.AddCommand(runActionCmd(g, kind))
	}
	plugins := &cobra.Command{Use: "plugin", Short: "Change invocation plugins and reopen Plan in a new revision"}
	for _, item := range []struct{ name, action string }{{"add", "plugin-attach"}, {"remove", "plugin-remove"}} {
		command := runActionCmd(g, item.action)
		command.Use = item.name + " <run>"
		plugins.AddCommand(command)
	}
	c.AddCommand(plugins)
	var after int64
	var follow bool
	events := &cobra.Command{Use: "events <run>", Short: "Read the durable event stream", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		for {
			events, err := client.Events(cmd.Context(), args[0], after)
			if err != nil {
				return err
			}
			for _, e := range events {
				if err = json.NewEncoder(cmd.OutOrStdout()).Encode(e); err != nil {
					return err
				}
				after = e.Sequence
			}
			if !follow {
				return nil
			}
			select {
			case <-cmd.Context().Done():
				return nil
			case <-time.After(250 * time.Millisecond):
			}
		}
	}}
	events.Flags().Int64Var(&after, "after", 0, "resume after this sequence")
	events.Flags().BoolVarP(&follow, "follow", "f", false, "follow events without owning execution")
	c.AddCommand(events)
	return c
}
func runActionCmd(g *globals, action string) *cobra.Command {
	req := daemon.ActionRequest{Action: action}
	var pluginFile string
	var configFile string
	c := &cobra.Command{Use: action + " <run>", Short: action + " a workflow", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		client, err := connect(cmd.Context(), g)
		if err != nil {
			return err
		}
		run, err := client.Get(cmd.Context(), args[0])
		if err != nil {
			return err
		}
		if req.ExpectedVersion == 0 {
			req.ExpectedVersion = run.Version
		}
		if req.Revision == "" {
			req.Revision = run.CurrentRevision
		}
		if req.OperationID == "" {
			req.OperationID = workflow.ID("op")
		}
		if action == "plugin-attach" {
			ref, err := plugin.ReadRef(pluginFile)
			if err != nil {
				return err
			}
			req.Plugin = &ref
		}
		if configFile != "" {
			path, err := filepath.Abs(configFile)
			if err != nil {
				return err
			}
			raw, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			config, err := workflow.Parse(raw)
			if err != nil {
				return err
			}
			config.Dir = filepath.Dir(path)
			req.Config = &config
		}
		result, err := client.Action(cmd.Context(), run.ID, req)
		if err != nil {
			return err
		}
		return printRun(cmd, g, result)
	}}
	c.Flags().StringVar(&req.OperationID, "operation-id", "", "idempotency key")
	c.Flags().Int64Var(&req.ExpectedVersion, "expected-version", 0, "reject changes since this version")
	c.Flags().StringVar(&req.Revision, "revision", "", "target revision (defaults to current)")
	switch action {
	case "plugin-attach":
		c.Flags().StringVar(&pluginFile, "file", "", "plugin reference YAML; adds or replaces the same plugin ID")
		_ = c.MarkFlagRequired("file")
	case "plugin-remove":
		c.Flags().StringVar(&req.PluginID, "id", "", "invocation plugin ID to remove")
		_ = c.MarkFlagRequired("id")
	case "message":
		c.Flags().StringVar(&req.Message, "text", "", "message body")
		_ = c.MarkFlagRequired("text")
		c.Flags().StringVar(&req.Recipient, "to", "supervisor", "supervisor or worker")
		c.Flags().StringVar(&req.Node, "node", "", "stage to address")
	case "rewind":
		c.Flags().StringVar(&req.Node, "to", "", "stage to revisit")
		_ = c.MarkFlagRequired("to")
		c.Flags().StringVar(&req.Task, "task", "", "amended objective (invalidates Task onward)")
		c.Flags().StringVar(&configFile, "config", "", "complete v2 configuration for this revision (reopens Plan)")
	case "approve":
		c.Flags().StringVar(&req.Attempt, "attempt", "", "attempt awaiting approval")
		_ = c.MarkFlagRequired("attempt")
		c.Flags().StringVar(&req.WorkDigest, "digest", "", "exact reviewed work digest")
		_ = c.MarkFlagRequired("digest")
		c.Flags().StringVar(&req.Actor, "actor", os.Getenv("USER"), "reviewer identity")
	case "priority":
		c.Flags().IntVar(&req.Priority, "value", 0, "priority (higher runs first)")
	}
	return c
}
func printRun(cmd *cobra.Command, g *globals, r *workflow.Run) error {
	if g.jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(r)
	}
	out := cmd.OutOrStdout()
	if _, err := fmt.Fprintf(out, "%s  %s  %s\n  %s\n", r.ID, r.Name, r.Current().State, r.Description); err != nil {
		return err
	}
	rev := r.Current()
	for _, a := range rev.Attempts {
		p := a.Progress
		if a.State != "running" || p == nil {
			continue
		}
		line := fmt.Sprintf("  %s attempt %d: %s", a.Node, a.Number, p.Phase)
		if p.Generation > 0 {
			line += fmt.Sprintf(" (resume %d)", p.Generation)
		}
		if p.Detail != "" {
			line += " — " + p.Detail
		}
		fmt.Fprintln(out, line)
		for _, activity := range p.Activity[max(0, len(p.Activity)-5):] {
			fmt.Fprintln(out, "    "+activity)
		}
	}
	for _, m := range rev.Messages {
		node := m.Node
		if node == "" {
			node = "any stage"
		}
		fmt.Fprintf(out, "  message to %s (%s): %s [%s]\n", m.Recipient, node, m.Body, rev.MessageStatus(m))
	}
	return printLimits(cmd, rev)
}

type nodeLimit struct {
	MaxAttempts    int `json:"max_attempts"`
	AttemptSeconds int `json:"attempt_seconds"`
	StallSeconds   int `json:"stall_seconds"`
	Attempts       int `json:"attempts"`
}

// nodeLimits reports each node's effective budget and the attempts it used.
func nodeLimits(rev *workflow.Revision) map[string]nodeLimit {
	out := map[string]nodeLimit{}
	for id := range rev.Config.Workflow.Nodes {
		l := rev.Config.NodeLimits(id)
		out[id] = nodeLimit{MaxAttempts: l.MaxAttempts, AttemptSeconds: l.AttemptSeconds, StallSeconds: rev.Config.StallSeconds(id)}
	}
	for _, a := range rev.Attempts {
		if l, ok := out[a.Node]; ok {
			l.Attempts++
			out[a.Node] = l
		}
	}
	return out
}
func printLimits(cmd *cobra.Command, rev *workflow.Revision) error {
	order, err := rev.Config.Workflow.Order()
	if err != nil {
		return err
	}
	limits := nodeLimits(rev)
	for _, id := range order {
		l := limits[id]
		if _, err = fmt.Fprintf(cmd.OutOrStdout(), "  %-12s %d of %d attempts, %ds per attempt, intervene after %ds without output\n", id, l.Attempts, l.MaxAttempts, l.AttemptSeconds, l.StallSeconds); err != nil {
			return err
		}
	}
	return nil
}
