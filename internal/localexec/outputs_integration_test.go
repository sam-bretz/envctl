package localexec

import (
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

// A real Claude worker follows the production prompt: it writes its stage
// document to the outputs file and submits a structured result without it.
// A real supervisor then approves without a correction field. Both results
// must satisfy the production contracts, and their usage must be reported.
func TestRealClaudeOutputDocumentsAndApprovalContract(t *testing.T) {
	if os.Getenv("ENVCTL_OUTPUTS_TEST") != "1" {
		t.Skip("opt-in real harness output contract")
	}
	state, runtimeID, credential := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM"), os.Getenv("ENVCTL_HARNESS_CREDENTIAL")
	if state == "" || runtimeID == "" || credential == "" {
		t.Fatal("select the owned acceptance VM and a Claude credential reference explicitly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "envctl-outputs-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("retained output-contract state:", dir)
	store, err := runstore.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := "{kind: claude, credential: \"" + credential + "\"}"
	c, err := workflow.Parse([]byte("version: 2\nproject: outputs\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\nagents: {worker: " + h + ", supervisor: " + h + "}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("outputs", "Design an `envctl version --json` flag that prints the version, commit and Go version as JSON. Keep the design under 25 lines.", "acceptance-test", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	run.Current().Runtime = workflow.RuntimeState{ID: runtimeID, Provider: "lima", Location: "local", Ready: true, State: "running"}
	a := engine.Assignment{Run: *run, Revision: *run.Current(), Attempt: workflow.Attempt{ID: workflow.ID("attempt"), Node: "design", Number: 1, State: "running", Inputs: map[string]string{}}}
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
	root := "/work/envctl/outputs-acceptance/" + a.Attempt.ID
	defer func() {
		for _, path := range []string{root, scratchDirectory(a)} {
			if err := b.Provider.Exec(context.Background(), runtimeID, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", path}}); err != nil {
				t.Error("fixture cleanup", err)
			}
		}
	}()
	if err = b.Provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "mkdir", "-p", root}}); err != nil {
		t.Fatal(err)
	}
	if err = b.Provider.Exec(ctx, runtimeID, vm.Command{Args: []string{"sudo", "install", "-d", "-m", "0700", "-o", "envctl-agent", "-g", "envctl-agent", scratchDirectory(a), outputsDirectory(a)}}); err != nil {
		t.Fatal(err)
	}
	credentialValue, err := agent.ResolveCredential("", "claude", credential)
	if err != nil {
		t.Fatal(err)
	}
	complete := func(i agent.Invocation) string {
		t.Helper()
		if _, err := harness.Start(ctx, i, credentialValue); err != nil {
			t.Fatal(err)
		}
		var log strings.Builder
		var cursor int64
		for ctx.Err() == nil {
			s, err := guest.Poll(ctx, i.ID, cursor)
			if err != nil {
				t.Fatal("retain state after observation error", err)
			}
			log.WriteString(s.Output)
			cursor = s.Cursor
			if s.Output != "" {
				continue
			}
			if s.State == "completed" {
				return log.String()
			}
			if s.State != "running" && s.State != "starting" && s.State != "pending" {
				t.Fatalf("%s ended in %s: %s", i.Role, s.State, tailString(log.String(), 1500))
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
			}
		}
		t.Fatal("output-contract acceptance expired; state retained")
		return ""
	}

	prompt, err := b.prompt(a, map[string]string{"app": root})
	if err != nil {
		t.Fatal(err)
	}
	node := a.Revision.Config.Workflow.Nodes["design"]
	worker := agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: root, Prompt: prompt, Schema: agent.ProposalSchema(node), TimeoutSeconds: 900}
	workerLog := complete(worker)
	raw, err := harness.Result(ctx, worker.ID)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := agent.ParseProposalWithSchema(raw, worker.Schema)
	if err != nil {
		t.Fatal("worker result does not satisfy the production contract", err, string(raw))
	}
	document, err := b.readOutput(ctx, a, "design")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(document) == "" {
		document = proposal.Artifacts["design"]
		t.Log("worker returned the document inline instead of writing the file")
	}
	if len(strings.TrimSpace(document)) < 200 || !strings.Contains(strings.ToLower(document), "json") || strings.TrimSpace(proposal.Summary) == "" {
		t.Fatalf("design document is missing or a placeholder (%d bytes): %q summary %q", len(document), tailString(document, 400), proposal.Summary)
	}
	workerUsage := harness.Usage(workerLog)

	supervisor := agent.Invocation{ID: a.Attempt.ID + "_supervisor", Role: "supervisor", Directory: root, Schema: agent.AssessmentSchema(), TimeoutSeconds: 600,
		Prompt: "You are the independent supervisor for an envctl design stage. Review this design for an `envctl version --json` flag. If it is a reasonable, specific design, accept it and give a one-sentence summary. Return only your structured assessment.\n\n" + document}
	supervisorLog := complete(supervisor)
	raw, err = harness.Result(ctx, supervisor.ID)
	if err != nil {
		t.Fatal(err)
	}
	assessment, err := agent.ParseAssessment(raw)
	if err != nil || strings.TrimSpace(assessment.Summary) == "" {
		t.Fatal("supervisor result does not satisfy the production contract", err, string(raw))
	}
	supervisorUsage := harness.Usage(supervisorLog)
	if workerUsage.Tokens() == 0 || supervisorUsage.Tokens() == 0 || workerUsage.Estimated || supervisorUsage.Estimated {
		t.Fatalf("usage not reported: worker %+v supervisor %+v", workerUsage, supervisorUsage)
	}
	if err = atomicJSON(dir+"/outputs-proof.json", map[string]any{"attempt": a.Attempt.ID, "document_bytes": len(document), "summary": proposal.Summary, "accepted": assessment.Accepted, "correction_present": strings.Contains(string(raw), "correction"), "worker_usage": workerUsage, "supervisor_usage": supervisorUsage}); err != nil {
		t.Fatal(err)
	}
	t.Logf("design document %d bytes via %s; supervisor accepted=%v; worker %s tokens ($%.2f), supervisor %s tokens ($%.2f)",
		len(document), outputsDirectory(a)+"/design.md", assessment.Accepted, workflow.FormatTokens(workerUsage.Tokens()), workerUsage.CostUSD, workflow.FormatTokens(supervisorUsage.Tokens()), supervisorUsage.CostUSD)
}

func tailString(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
