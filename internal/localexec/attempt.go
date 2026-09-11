package localexec

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/repository"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type attemptRecord struct {
	Binding       string               `json:"binding"`
	Phase         string               `json:"phase"`
	Directories   map[string]string    `json:"directories"`
	Worker        agent.Invocation     `json:"worker"`
	Supervisor    *agent.Invocation    `json:"supervisor,omitempty"`
	Stack         *gueststack.Prepared `json:"stack,omitempty"`
	Result        workflow.Result      `json:"result"`
	CheckIndex    int                  `json:"check_index"`
	Cursors       map[string]int64     `json:"cursors"`
	Logs          map[string]string    `json:"logs"`
	Session       string               `json:"session,omitempty"`
	Detail        string               `json:"detail,omitempty"`
	Recovery      *recoverySource      `json:"recovery,omitempty"`
	RecoveredFrom string               `json:"recovered_from,omitempty"`
	// Steering lists user messages per agent role and generation. At stays zero
	// until the job carrying the message has been submitted.
	Steering          []workflow.Delivery  `json:"steering,omitempty"`
	Live              map[string]*liveRole `json:"live,omitempty"`
	SupervisorSession string               `json:"supervisor_session,omitempty"`
	// Stall is the durable no-output watch per agent role.
	Stall map[string]*stallState `json:"stall,omitempty"`
}

// Recovery source preserves interrupted work without creating a proposal,
// supervisor acceptance, or checkpoint. The next attempt must verify it anew.
type recoverySource struct {
	Commits       map[string]string            `json:"commits"`
	Sources       map[string]workflow.Artifact `json:"sources"`
	SourceObjects map[string]workflow.Artifact `json:"source_objects,omitempty"`
}

func attemptBinding(a engine.Assignment) string {
	return workflow.Digest(struct {
		Runtime, Revision, Attempt, Node, Objective string
		Config                                      workflow.Config
		Inputs                                      map[string]string
	}{a.Revision.Runtime.ID, a.Revision.ID, a.Attempt.ID, a.Attempt.Node, a.Revision.Objective, a.Revision.Config, a.Attempt.Inputs})
}
func atomicJSON(filename string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(filename), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(filename), ".writing-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filename); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(filename))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func (b *Backend) recordPath(a engine.Assignment) (string, error) {
	dir, err := b.dir(a)
	if err != nil {
		return "", err
	}
	if !idPattern.MatchString(a.Attempt.ID) {
		return "", errors.New("invalid attempt identity")
	}
	return filepath.Join(dir, "attempts", a.Attempt.ID+".json"), nil
}
func (b *Backend) load(a engine.Assignment) (*attemptRecord, error) {
	filename, err := b.recordPath(a)
	if err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filename)
	if err != nil {
		return nil, err
	}
	var r attemptRecord
	if json.Unmarshal(raw, &r) != nil || r.Binding != attemptBinding(a) {
		return nil, errors.New("attempt receipt does not match its immutable assignment")
	}
	if r.Result.Sources == nil {
		r.Result.Sources = map[string]workflow.Artifact{}
	}
	if r.Result.SourceObjects == nil {
		r.Result.SourceObjects = map[string]workflow.Artifact{}
	}
	return &r, nil
}
func (b *Backend) save(a engine.Assignment, r *attemptRecord) error {
	filename, err := b.recordPath(a)
	if err != nil {
		return err
	}
	return atomicJSON(filename, r)
}
func (b *Backend) currentStack(a engine.Assignment) (gueststack.Prepared, error) {
	dir, err := b.dir(a)
	if err != nil {
		return gueststack.Prepared{}, err
	}
	raw, err := os.ReadFile(filepath.Join(dir, "stack.json"))
	if err != nil {
		return gueststack.Prepared{}, err
	}
	var p gueststack.Prepared
	if json.Unmarshal(raw, &p) != nil {
		return p, errors.New("invalid active stack receipt")
	}
	return p, nil
}
func (b *Backend) saveStack(a engine.Assignment, p gueststack.Prepared) error {
	dir, err := b.dir(a)
	if err != nil {
		return err
	}
	return atomicJSON(filepath.Join(dir, "stack.json"), p)
}

func (b *Backend) Start(ctx context.Context, a engine.Assignment) error {
	if err := assignmentRuntime(a); err != nil {
		return err
	}
	if _, err := b.load(a); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	pins, err := inputCommits(a)
	if err != nil {
		return err
	}
	if err := b.prepareJoin(ctx, a); err != nil {
		return err
	}
	var resume string
	var recoveredFrom string
	// A failed check or supervisor correction resumes the real worker session
	// against its captured work, in a new assignment worktree. The previous
	// attempt remains immutable and retains its complete failure evidence.
	for i := len(a.Revision.Attempts) - 1; i >= 0; i-- {
		prior := a.Revision.Attempts[i]
		if prior.Node != a.Attempt.Node || prior.State != "failed" || workflow.Digest(prior.Inputs) != workflow.Digest(a.Attempt.Inputs) {
			continue
		}
		old := a
		old.Attempt = prior
		record, e := b.load(old)
		if e != nil {
			continue
		}
		commits, sources, objects := record.Result.Commits, record.Result.Sources, record.Result.SourceObjects
		if record.Recovery != nil {
			commits, sources, objects = record.Recovery.Commits, record.Recovery.Sources, record.Recovery.SourceObjects
		}
		if len(commits) != len(a.Revision.Config.Repositories) {
			continue
		}
		for _, repo := range a.Revision.Config.Repositories {
			if artifact, ok := sources[repo.ID]; ok {
				raw, e := b.Store.Artifact(artifact.Digest)
				if e != nil {
					return e
				}
				if e = b.repos(a).RestoreBundle(ctx, repo.ID, commits[repo.ID], bytes.NewReader(raw)); e != nil {
					return errors.New("failed-attempt source restoration failed")
				}
			} else if writable(a.Revision.Config.Workflow.Nodes[a.Attempt.Node], repo.ID) {
				return errors.New("failed worker recovery is missing writable source evidence")
			}
			if artifact, ok := objects[repo.ID]; ok {
				raw, e := b.Store.Artifact(artifact.Digest)
				if e != nil {
					return e
				}
				if e = b.repos(a).RestoreObjects(ctx, repo.ID, commits[repo.ID], bytes.NewReader(raw)); e != nil {
					return errors.New("failed-attempt submodule or LFS restoration failed")
				}
			}
		}
		pins = workflow.Clone(commits)
		resume = record.Session
		recoveredFrom = prior.ID
		break
	}
	dirs, err := b.worktrees(ctx, a, a.Attempt.ID, pins)
	if err != nil {
		return err
	}
	node := a.Revision.Config.Workflow.Nodes[a.Attempt.Node]
	root := dirs[a.Revision.Config.Repositories[0].ID]
	if err := b.Provider.Exec(ctx, a.Revision.Runtime.ID, vm.Command{Args: []string{"sudo", "install", "-d", "-m", "0700", "-o", "envctl-agent", "-g", "envctl-agent", scratchDirectory(a)}, Stderr: io.Discard}); err != nil {
		return errors.New("stage scratch directory preparation failed")
	}
	prompt, err := b.prompt(a, dirs)
	if err != nil {
		return err
	}
	if recoveredFrom != "" {
		prompt += "\nThis worktree restores unfinished work from attempt " + recoveredFrom + ". It has no accepted checkpoint. Inspect it, continue the task, and correct the recorded failure before returning a complete proposal.\n"
	}
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "worker", Directories: dirs, Cursors: map[string]int64{}, Logs: map[string]string{}, Result: workflow.Result{Commits: map[string]string{}, Sources: map[string]workflow.Artifact{}, SourceObjects: map[string]workflow.Artifact{}}}
	r.RecoveredFrom = recoveredFrom
	r.Worker = agent.Invocation{ID: a.Attempt.ID + "_worker", Role: "worker", Directory: root, Prompt: prompt, Schema: agent.ProposalSchema(node), Model: a.Revision.Config.Agents.Worker.Model, TimeoutSeconds: a.Revision.Config.NodeLimits(a.Attempt.Node).AttemptSeconds}
	r.Worker.Session = resume
	// prompt() carries every worker-directed message of the revision.
	r.include(a, "worker", func(m workflow.Message) bool { return m.Recipient == "worker" })
	if node.Kind != "task" && node.Kind != "plan" {
		p, err := b.stack(a).Prepare(ctx, stackSpec(a, a.Attempt.ID+"_before", dirs))
		if err != nil {
			return err
		}
		if !b.dataBusy(a) {
			if err = b.stack(a).Up(ctx, p, 90); err != nil {
				return err
			}
		}
		r.Stack = &p
		if err = b.saveStack(a, p); err != nil {
			return err
		}
		if err = b.restoreDataInputs(ctx, a, p); err != nil {
			return err
		}
	}
	// Freeze instructions before submitting anything. Reconciliation can safely
	// recreate a missing guest job from this same record after daemon restart.
	return b.save(a, r)
}

func (b *Backend) prompt(a engine.Assignment, dirs map[string]string) (string, error) {
	node := a.Revision.Config.Workflow.Nodes[a.Attempt.Node]
	var text strings.Builder
	fmt.Fprintf(&text, "You are the worker for envctl stage %q (kind %s).\nObjective: %s\nStage instructions: %s\nRepository worktrees: %s\nWritable repositories: %s\n", a.Attempt.Node, node.Kind, a.Revision.Objective, node.Prompt, mustJSON(dirs), mustJSON(node.Writes))
	text.WriteString("Complete this stage and return the required structured artifacts. Use tools to inspect and implement. Implementation changes belong inside the assigned writable worktrees. Do not modify source in other attempts, agent homes, orchestration files, or VM configuration. Do not push, create a PR, or approve changes. The coordinator runs checks and a separate supervisor reviews your proposal. For read-only repositories do not create, remove, or edit tracked or untracked files; adding a report directory also violates the read-only contract. Return documentation in the structured artifacts response. Plan must enumerate downstream capabilities, tests, datasets, harness and publication requirements and resolve scope against the objective.\n")
	fmt.Fprintf(&text, "Temporary reports, screenshots and test output may be written to this attempt's private scratch directory: %s. It is outside the source checkpoints. Copy any evidence that should be retained into your structured artifacts response.\n", scratchDirectory(a))
	text.WriteString("Workflow definition:\n" + string(mustJSON(a.Revision.Config.Workflow)) + "\nReadiness:\n" + string(mustJSON(a.Revision.Readiness)) + "\n")
	if node.Join != nil {
		parents, err := a.Revision.MergeInputs(a.Attempt.Node)
		if err != nil {
			return "", err
		}
		text.WriteString("Explicit join policy:\n" + string(mustJSON(node.Join)) + "\nRequired repository merge ancestors:\n" + string(mustJSON(parents)) + "\n")
		if len(parents) > 0 {
			text.WriteString("Your writable worktree starts at the declared repository input (or recovered unfinished work). Merge EVERY listed input SHA into that repository using Git. Resolve conflicts according to the accepted plan and design. Do not squash or replace history. Retained output bundles are independently checked for all input ancestors; executable checks and the supervisor judge the merged content. Other inputs have offline source copies for inspection and nested-module/LFS conflict resolution. These copies must not be modified.\n")
			for _, parent := range node.Needs {
				for _, repo := range a.Revision.Config.Repositories {
					dir, err := b.repos(a).WorktreeDir(joinInputID(a, parent), repo.ID)
					if err != nil {
						return "", err
					}
					fmt.Fprintf(&text, "Join input %s repository %s: %s\n", parent, repo.ID, dir)
				}
			}
		}
	}
	if node.Kind == "plan" {
		text.WriteString("Return a structured requirements array naming every capability you discover the workflow needs, with affected node IDs and a reason. Use the existing capability names when applicable. Include additional missing connections; do not claim they are ready. An empty array means no additional requirements discovered. The coordinator always enforces configured/built-in requirements independently. Resolve contradictions in the spec before proposing it; the supervisor must reject an incomplete inventory.\n")
		text.WriteString("Current mandatory capability inventory:\n" + string(mustJSON(a.Revision.Requirements())) + "\nPreviously discovered requirements:\n" + string(mustJSON(a.Revision.DiscoveredRequirements)) + "\nInvocation plugin bindings (credential references only):\n" + string(mustJSON(a.Revision.Config.Plugins)) + "\n")
	}
	if len(a.Revision.Config.Data.Datasets) > 0 {
		text.WriteString("Application dataset definitions and verification:\n" + string(mustJSON(a.Revision.Config.Data)) + "\nThe coordinator restores predecessor data before work and captures verified data after checks. Do not change dataset ownership, orchestration receipts, or snapshot files.\n")
	}
	ids := make([]string, 0, len(a.Revision.Checkpoints))
	for id := range a.Revision.Checkpoints {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		cp := a.Revision.Checkpoints[id]
		if !a.Revision.Config.Workflow.Descendants(id)[a.Attempt.Node] {
			continue
		}
		fmt.Fprintf(&text, "\nAccepted %s checkpoint %s: %s\n", id, cp.ID, cp.Result.Summary)
		for _, artifact := range cp.Result.Artifacts {
			if artifact.MediaType != "text/markdown" {
				continue
			}
			raw, err := b.Store.Artifact(artifact.Digest)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&text, "Artifact %s:\n%s\n", artifact.Name, raw)
		}
	}
	for _, prior := range a.Revision.Attempts {
		if prior.Node == a.Attempt.Node && prior.State == "failed" {
			fmt.Fprintf(&text, "Previous attempt failed: %s\n", prior.Error)
		}
	}
	for _, message := range a.Revision.Messages {
		if message.Recipient == "worker" || message.Recipient == "both" {
			fmt.Fprintf(&text, "User steering: %s\n", message.Body)
		}
	}
	if text.Len() > 512<<10 {
		return "", errors.New("stage context exceeds the configured adapter input bound")
	}
	return text.String(), nil
}

func scratchDirectory(a engine.Assignment) string {
	return "/work/envctl/scratch/" + a.Revision.ID + "/" + a.Attempt.ID
}

func (b *Backend) pollJob(ctx context.Context, a engine.Assignment, r *attemptRecord, id string) (guestjob.Status, error) {
	status, err := b.guest(a).Poll(ctx, id, r.Cursors[id])
	if err != nil {
		return status, err
	}
	if status.State == "pending" {
		if _, err := b.guest(a).Reconcile(ctx, id); err != nil {
			return status, err
		}
		// Read any newly available output with this attempt's own cursor on the
		// next poll, rather than replaying Reconcile's first output page.
		return status, nil
	}
	if status.Output != "" {
		r.Cursors[id] = status.Cursor
		r.Logs[id] += status.Output
		r.noteOutput(id, b.now())
		if len(r.Logs[id]) > 16<<20 {
			return status, errors.New("guest job evidence exceeds the journal bound")
		}
		for _, role := range []string{"worker", "supervisor"} {
			if r.session(role) != "" || !slices.Contains(r.jobs(role), id) {
				continue
			}
			h := a.Revision.Config.Agents.Worker
			if role == "supervisor" {
				h = a.Revision.Config.Agents.Supervisor
			}
			harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a))
			if err != nil {
				return status, err
			}
			if role == "supervisor" {
				r.SupervisorSession = harness.Session(r.Logs[id])
			} else {
				r.Session = harness.Session(r.Logs[id])
			}
		}
		if err = b.save(a, r); err != nil {
			return status, err
		}
		// Drain every page of terminal evidence before making a decision.
		if status.State == "completed" || status.State == "failed" || status.State == "timed-out" {
			status.State = "running"
		}
	}
	return status, nil
}
func pending(state string) bool {
	return state == "running" || state == "starting" || state == "pending"
}
func (b *Backend) failed(a engine.Assignment, r *attemptRecord, detail string) (engine.Observation, error) {
	r.Phase = "failed"
	r.Detail = detail
	if err := b.save(a, r); err != nil {
		return engine.Observation{}, err
	}
	return engine.Observation{State: "failed", Detail: detail, Session: r.Session, Delivered: r.delivered()}, nil
}

func (b *Backend) workerFailed(ctx context.Context, a engine.Assignment, r *attemptRecord, detail string) (engine.Observation, error) {
	if r.Recovery == nil {
		pins, err := inputCommits(a)
		if err != nil {
			return engine.Observation{}, err
		}
		recovery := &recoverySource{Commits: pins, Sources: map[string]workflow.Artifact{}, SourceObjects: map[string]workflow.Artifact{}}
		for _, repo := range a.Revision.Config.Repositories {
			if !writable(a.Revision.Config.Workflow.Nodes[a.Attempt.Node], repo.ID) {
				continue
			}
			pin, err := b.repos(a).Capture(ctx, a.Attempt.ID, repo.ID, true)
			if err != nil {
				return engine.Observation{}, errors.New("interrupted worker source capture needs recovery; its worktree is retained")
			}
			var bundle bytes.Buffer
			if err = b.repos(a).ExportBundle(ctx, a.Attempt.ID, repo.ID, pin, &bundle); err != nil {
				return engine.Observation{}, errors.New("interrupted worker source export needs recovery; its worktree is retained")
			}
			artifact, err := b.artifact(a, "source", "application/x-git-bundle", bundle.Bytes())
			if err != nil {
				return engine.Observation{}, err
			}
			recovery.Commits[repo.ID], recovery.Sources[repo.ID] = pin, artifact
			objects, err := b.sourceObjects(ctx, a, repo.ID, pin)
			if err != nil {
				return engine.Observation{}, err
			}
			recovery.SourceObjects[repo.ID] = objects
		}
		r.Recovery = recovery
	}
	return b.failed(a, r, detail+"; unfinished source retained for the next attempt")
}
func (b *Backend) startAgent(ctx context.Context, a engine.Assignment, i agent.Invocation) error {
	h := a.Revision.Config.Agents.Worker
	if i.Role == "supervisor" {
		h = a.Revision.Config.Agents.Supervisor
	}
	credential, err := agent.ResolveCredential(a.Revision.Config.Dir, h.Kind, h.Credential)
	if err != nil {
		return err
	}
	harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a))
	if err != nil {
		return err
	}
	_, err = harness.Start(ctx, i, credential)
	return err
}

func (b *Backend) agentResult(ctx context.Context, a engine.Assignment, role, id string) (json.RawMessage, error) {
	h := a.Revision.Config.Agents.Worker
	if role == "supervisor" {
		h = a.Revision.Config.Agents.Supervisor
	}
	harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a))
	if err != nil {
		return nil, err
	}
	return harness.Result(ctx, id)
}

func (b *Backend) Poll(ctx context.Context, a engine.Assignment) (engine.Observation, error) {
	if err := assignmentRuntime(a); err != nil {
		return engine.Observation{}, err
	}
	r, err := b.load(a)
	if errors.Is(err, os.ErrNotExist) {
		return engine.Observation{State: "missing"}, nil
	}
	if err != nil {
		return engine.Observation{}, err
	}
	running := func() (engine.Observation, error) {
		return engine.Observation{State: "running", Session: r.Session, Progress: b.progress(a, r), Delivered: r.delivered()}, nil
	}
	// agentJob reconciles one role's active generation: it finishes stopping a
	// superseded generation, starts a missing job, records delivery once the
	// job exists, and interrupts it when new steering arrives. It returns the
	// job status, or done=false while the role is still running.
	agentJob := func(role string) (status guestjob.Status, done bool, err error) {
		if busy, err := b.interrupt(ctx, a, r, role); busy || err != nil {
			return status, false, err
		}
		current := r.invocation(role)
		if status, err = b.pollJob(ctx, a, r, current.ID); err != nil {
			return status, false, err
		}
		if status.State == "missing" {
			if err = b.startAgent(ctx, a, *current); err != nil {
				return status, false, err
			}
		}
		if r.markDelivered(role) {
			if err = b.save(a, r); err != nil {
				return status, false, err
			}
		}
		if status.State == "missing" {
			return status, false, nil
		}
		// A stall decision is final for this attempt: stop the job, then fail.
		if r.stallFailure(role) != "" {
			return b.stopStalled(ctx, a, r, role, status)
		}
		// A completed job whose steering is undelivered is superseded too:
		// its proposal cannot be accepted without the user's message.
		if pending(status.State) || status.State == "completed" {
			steered, err := b.steer(a, r, role)
			if err != nil || steered {
				return status, false, err
			}
		}
		if pending(status.State) {
			if _, err = b.watchStall(a, r, role, r.Logs[current.ID]); err != nil {
				return status, false, err
			}
			if r.stallFailure(role) != "" {
				return b.stopStalled(ctx, a, r, role, status)
			}
			return status, false, nil
		}
		return status, true, nil
	}
	node := a.Revision.Config.Workflow.Nodes[a.Attempt.Node]
	switch r.Phase {
	case "worker":
		status, done, err := agentJob("worker")
		if err != nil {
			return engine.Observation{}, err
		}
		if !done {
			return running()
		}
		if status.State != "completed" {
			if stalled := r.stallFailure("worker"); stalled != "" {
				return b.workerFailed(ctx, a, r, stalled)
			}
			return b.workerFailed(ctx, a, r, "worker job ended in "+status.State+"; its journal is retained")
		}
		worker := r.invocation("worker")
		raw, err := b.agentResult(ctx, a, "worker", worker.ID)
		if err != nil {
			return engine.Observation{}, err
		}
		proposal, err := agent.ParseProposalWithSchema(clean(a, raw), worker.Schema)
		if err != nil {
			return b.workerFailed(ctx, a, r, err.Error())
		}
		r.Result.Summary = proposal.Summary
		r.Result.Data = proposal.Data
		if err := workflow.ValidateRequirements(a.Revision.Config.Workflow, proposal.Requirements); err != nil {
			return b.workerFailed(ctx, a, r, err.Error())
		}
		r.Result.Requirements = proposal.Requirements
		for _, name := range node.Outputs {
			artifact, err := b.artifact(a, name, "text/markdown", []byte(proposal.Artifacts[name]))
			if err != nil {
				return engine.Observation{}, err
			}
			r.Result.Artifacts = append(r.Result.Artifacts, artifact)
		}
		for _, repo := range a.Revision.Config.Repositories {
			pin, err := b.repos(a).Capture(ctx, a.Attempt.ID, repo.ID, writable(node, repo.ID))
			if err != nil {
				return b.failed(a, r, "worker modified a read-only repository or commit capture failed: "+string(clean(a, []byte(err.Error()))))
			}
			r.Result.Commits[repo.ID] = pin
			var bundle bytes.Buffer
			if err = b.repos(a).ExportBundle(ctx, a.Attempt.ID, repo.ID, pin, &bundle); err != nil {
				return engine.Observation{}, errors.New("checkpoint source export failed")
			}
			artifact, err := b.artifact(a, "source", "application/x-git-bundle", bundle.Bytes())
			if err != nil {
				return engine.Observation{}, err
			}
			r.Result.Sources[repo.ID] = artifact
			objects, err := b.sourceObjects(ctx, a, repo.ID, pin)
			if err != nil {
				return engine.Observation{}, err
			}
			r.Result.SourceObjects[repo.ID] = objects
		}
		if err := b.verifyJoin(ctx, a, &r.Result); err != nil {
			if ctx.Err() != nil {
				return engine.Observation{}, ctx.Err()
			}
			return b.failed(a, r, err.Error())
		}
		r.Phase = "stack"
		if err = b.save(a, r); err != nil {
			return engine.Observation{}, err
		}
		return running()
	case "stack":
		// Task/Plan can run while Compose requirements are being resolved. Later
		// stages verify their own checked-out source with the actual stack.
		if node.Kind != "task" && node.Kind != "plan" {
			p, err := b.stack(a).Prepare(ctx, stackSpec(a, a.Attempt.ID+"_after", r.Directories))
			if err != nil {
				return b.failed(a, r, "stage Compose preparation failed")
			}
			if err = b.stack(a).Up(ctx, p, 90); err != nil {
				return b.failed(a, r, "stage Compose services did not pass their health checks")
			}
			r.Stack = &p
			if err = b.saveStack(a, p); err != nil {
				return engine.Observation{}, err
			}
		}
		r.Phase = "checks"
		if err = b.save(a, r); err != nil {
			return engine.Observation{}, err
		}
		return running()
	case "checks":
		if r.CheckIndex < len(node.Checks) {
			check := node.Checks[r.CheckIndex]
			if check.Plugin != "" {
				record, err := b.pluginCheck(ctx, a, check, r.CheckIndex)
				if err != nil {
					return engine.Observation{}, err
				}
				if record.Response == nil || !record.Response.OK {
					return b.failed(a, r, "plugin check "+check.Name+" failed; evidence "+record.Evidence.Digest)
				}
				r.Result.Checks = append(r.Result.Checks, workflow.CheckResult{Name: check.Name, Passed: true, ExitCode: 0, EvidenceDigest: record.Evidence.Digest, CommitsDigest: workflow.Digest(r.Result.Commits)})
				r.CheckIndex++
				if err = b.save(a, r); err != nil {
					return engine.Observation{}, err
				}
				return running()
			}
			id := fmt.Sprintf("%s_check_%d", a.Attempt.ID, r.CheckIndex)
			status, err := b.pollJob(ctx, a, r, id)
			if err != nil {
				return engine.Observation{}, err
			}
			if status.State == "missing" {
				repo := check.Repository
				if repo == "" {
					repo = a.Revision.Config.Repositories[0].ID
				}
				dir := r.Directories[repo]
				if dir == "" {
					return b.failed(a, r, "check references an unavailable repository")
				}
				timeout := check.TimeoutSeconds
				if timeout < 1 {
					timeout = a.Revision.Config.NodeLimits(a.Attempt.Node).AttemptSeconds
				}
				_, err = b.guest(a).Submit(ctx, guestjob.Request{ID: id, Args: check.Command, Dir: dir, Env: map[string]string{"PYTHONDONTWRITEBYTECODE": "1"}, Secrets: secrets(a), TimeoutSeconds: timeout})
				if err != nil {
					return engine.Observation{}, err
				}
				return running()
			}
			if pending(status.State) {
				return running()
			}
			evidence, err := b.artifact(a, "check."+check.Name, "text/plain", []byte(r.Logs[id]))
			if err != nil {
				return engine.Observation{}, err
			}
			if status.State != "completed" || status.ExitCode == nil || *status.ExitCode != 0 {
				return b.failed(a, r, "check "+check.Name+" failed; evidence "+evidence.Digest+"\n"+tail(r.Logs[id], 12000))
			}
			r.Result.Checks = append(r.Result.Checks, workflow.CheckResult{Name: check.Name, Passed: true, ExitCode: 0, EvidenceDigest: evidence.Digest, CommitsDigest: workflow.Digest(r.Result.Commits)})
			r.CheckIndex++
			if err = b.save(a, r); err != nil {
				return engine.Observation{}, err
			}
			return running()
		}
		if node.Kind == "qa" && len(node.Checks) == 0 {
			return b.failed(a, r, "QA requires executable acceptance checks in the plan")
		}
		if err = b.unchanged(ctx, a, r); err != nil {
			return b.failed(a, r, "verification commands changed the proposed repository commits")
		}
		if node.Kind != "task" && node.Kind != "plan" {
			if err = b.captureData(ctx, a, r); err != nil {
				return engine.Observation{}, err
			}
		}
		prompt := "The following is the worker's frozen assignment and context for your review:\n" + r.Worker.Prompt + "\nWorker result and coordinator check receipts:\n" + string(mustJSON(r.Result))
		prompt += "\nLatest volatile readiness observations (these may have changed since the frozen assignment; distinguish later drift from inaccurate original evidence):\n" + string(mustJSON(a.Revision.Readiness))
		for _, artifact := range r.Result.Artifacts {
			if artifact.MediaType == "text/markdown" {
				raw, err := b.Store.Artifact(artifact.Digest)
				if err != nil {
					return engine.Observation{}, err
				}
				prompt += "\n" + artifact.Name + ":\n" + string(raw)
			}
		}
		steering := ""
		for _, m := range a.Revision.Messages {
			if m.Targets(a.Attempt.Node, "supervisor") {
				steering += "- to the " + m.Recipient + ": " + m.Body + "\n"
			}
		}
		if steering != "" {
			prompt += "\nUser steering for this stage (reject the work if it does not address steering addressed to the worker):\n" + steering
		}
		prompt += "\nYour role is the independent supervisor, not the worker described above. Review alignment with the task, accepted plan and design, actual source, and test evidence. Do not modify files or repeat the implementation. Reject incomplete work with a specific correction. Do not reject Task or Plan for known readiness items that the coordinator is still resolving: assess artifact completeness and report required capabilities. The coordinator separately enforces executable readiness. Return only your structured assessment."
		r.Supervisor = &agent.Invocation{ID: a.Attempt.ID + "_supervisor", Role: "supervisor", Directory: r.Worker.Directory, Prompt: prompt, Schema: agent.AssessmentSchema(), Model: a.Revision.Config.Agents.Supervisor.Model, TimeoutSeconds: a.Revision.Config.NodeLimits(a.Attempt.Node).AttemptSeconds}
		r.include(a, "supervisor", func(m workflow.Message) bool { return m.Targets(a.Attempt.Node, "supervisor") })
		r.Phase = "supervisor"
		if err = b.save(a, r); err != nil {
			return engine.Observation{}, err
		}
		return running()
	case "supervisor":
		if r.Supervisor == nil {
			return engine.Observation{}, errors.New("supervisor request missing from attempt receipt")
		}
		status, done, err := agentJob("supervisor")
		if err != nil {
			return engine.Observation{}, err
		}
		if !done {
			return running()
		}
		if status.State != "completed" {
			if stalled := r.stallFailure("supervisor"); stalled != "" {
				return b.failed(a, r, stalled)
			}
			return b.failed(a, r, "supervisor job ended in "+status.State)
		}
		raw, err := b.agentResult(ctx, a, "supervisor", r.invocation("supervisor").ID)
		if err != nil {
			return engine.Observation{}, err
		}
		assessment, err := agent.ParseAssessment(clean(a, raw))
		if err != nil {
			return b.failed(a, r, err.Error())
		}
		evidence, err := b.artifact(a, "supervisor-review", "application/json", raw)
		if err != nil {
			return engine.Observation{}, err
		}
		if !assessment.Accepted {
			return b.failed(a, r, "supervisor correction: "+assessment.Correction+"\n"+assessment.Summary)
		}
		if err = b.unchanged(ctx, a, r); err != nil {
			return b.failed(a, r, "supervisor altered the proposed repository commits")
		}
		r.Result.Review = workflow.Review{Accepted: true, Summary: assessment.Summary, EvidenceDigest: evidence.Digest, ResultDigest: r.Result.WorkDigest()}
		r.Phase = "completed"
		if err = b.save(a, r); err != nil {
			return engine.Observation{}, err
		}
		return engine.Observation{State: "completed", Result: &r.Result, Session: r.Session, Delivered: r.delivered()}, nil
	case "completed":
		return engine.Observation{State: "completed", Result: &r.Result, Session: r.Session, Delivered: r.delivered()}, nil
	case "failed":
		return engine.Observation{State: "failed", Detail: r.Detail, Session: r.Session, Delivered: r.delivered()}, nil
	default:
		return engine.Observation{}, errors.New("unknown durable attempt phase")
	}
}
func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
func (b *Backend) unchanged(ctx context.Context, a engine.Assignment, r *attemptRecord) error {
	for _, repo := range a.Revision.Config.Repositories {
		pin, err := b.repos(a).Capture(ctx, a.Attempt.ID, repo.ID, true)
		if err != nil || pin != r.Result.Commits[repo.ID] {
			return errors.New("proposed commits changed during verification")
		}
	}
	return nil
}

func (b *Backend) sourceObjects(ctx context.Context, a engine.Assignment, repositoryID, commit string) (workflow.Artifact, error) {
	var objects bytes.Buffer
	if err := b.repos(a).ExportObjects(ctx, a.Attempt.ID, repositoryID, commit, &objects); err != nil {
		return workflow.Artifact{}, errors.New("checkpoint submodule or LFS capture failed; source is retained for recovery")
	}
	return b.artifact(a, "source-objects", repository.ObjectsMediaType, objects.Bytes())
}
