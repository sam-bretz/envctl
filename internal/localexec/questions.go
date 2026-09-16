package localexec

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/sam-bretz/envctl/internal/agent"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// The engine finds this through a type assertion, so a signature drift would
// quietly turn every question into "cannot answer" instead of failing to
// build.
var _ engine.QuestionBackend = (*Backend)(nil)

// answerTimeoutSeconds bounds one answer. A question is a read, not a stage,
// so it gets far less time than an attempt.
const answerTimeoutSeconds = 600

// Answer answers a question about a stage with a separate, read-only
// invocation of that stage's supervisor. It never touches the attempt's own
// jobs or record: the worker keeps running and the review stays as it was.
//
// It follows the shape of a readiness probe: the job ID is derived from the
// question, so a restart finds the job it already started instead of asking
// twice.
func (b *Backend) Answer(ctx context.Context, a engine.Assignment, q workflow.Question) (engine.QuestionObservation, error) {
	h := a.Revision.Config.Agents.Supervisor
	harness, err := agent.SelectVersion(h.Kind, h.Version, b.guest(a))
	if err != nil {
		return unanswerable("the supervisor's harness is unavailable"), nil
	}
	id := q.ID + "_answer"
	status, err := b.guest(a).Poll(ctx, id, 0)
	if err != nil {
		return engine.QuestionObservation{}, err
	}
	switch {
	case status.State == "missing":
		return b.startAnswer(ctx, a, q, id, h, harness)
	case status.State == "pending":
		_, err = b.guest(a).Reconcile(ctx, id)
		return engine.QuestionObservation{State: "running"}, err
	case pending(status.State):
		return engine.QuestionObservation{State: "running"}, nil
	}
	usage := harness.Usage(status.Output)
	if status.State != "completed" {
		return engine.QuestionObservation{State: workflow.QuestionFailed, Detail: "answering ended in " + status.State, Usage: &usage}, nil
	}
	raw, err := harness.Result(ctx, id)
	if err != nil {
		return engine.QuestionObservation{State: workflow.QuestionFailed, Detail: "the supervisor's answer could not be read", Usage: &usage}, nil
	}
	answer, err := agent.ParseAnswer(clean(a, raw))
	if err != nil {
		return engine.QuestionObservation{State: workflow.QuestionFailed, Detail: "the supervisor did not return an answer: " + err.Error(), Usage: &usage}, nil
	}
	return engine.QuestionObservation{State: workflow.QuestionAnswered, Answer: answer, Usage: &usage}, nil
}

func (b *Backend) startAnswer(ctx context.Context, a engine.Assignment, q workflow.Question, id string, h workflow.Harness, harness agent.Harness) (engine.QuestionObservation, error) {
	record, err := b.load(a)
	if err != nil || record.Worker.Directory == "" {
		return unanswerable("the stage's worktree is no longer available, so its supervisor cannot look at the work"), nil
	}
	credential, err := agent.ResolveCredential(a.Revision.Config.Dir, h.Kind, h.Credential)
	if err != nil {
		return unanswerable("the supervisor's harness credential is unavailable"), nil
	}
	if _, err = harness.Start(ctx, answerInvocation(a, record, q, id, h), credential); err != nil {
		return engine.QuestionObservation{}, err
	}
	return engine.QuestionObservation{State: "running"}, nil
}

// answerInvocation is the read-only invocation that answers a question.
func answerInvocation(a engine.Assignment, record *attemptRecord, q workflow.Question, id string, h workflow.Harness) agent.Invocation {
	i := agent.Invocation{
		ID: id, Role: "supervisor", Directory: record.Worker.Directory,
		Prompt: questionPrompt(a, record, q), Schema: agent.AnswerSchema(),
		// The stage's own supervisor model, so the answer comes from the
		// same kind of reviewer that judged the work.
		Model:          a.Revision.Config.NodeAgents(q.Node).Supervisor.Model,
		TimeoutSeconds: answerTimeoutSeconds, ReadOnly: true,
	}
	// Answering from the supervisor's own session grounds the answer in what
	// it actually reviewed. Only Claude can branch that session; continuing it
	// would append the question to the transcript the supervisor resumes when
	// steered, so Codex starts fresh from the facts in the prompt instead.
	if h.Kind == "claude" && record.SupervisorSession != "" {
		i.Session, i.Fork = record.SupervisorSession, true
	}
	return i
}

func unanswerable(detail string) engine.QuestionObservation {
	return engine.QuestionObservation{State: workflow.QuestionFailed, Detail: detail}
}

// questionPrompt gives the supervisor what it needs to answer from facts
// rather than memory: the stage, the work and its review, the checks bound to
// commits, any pull requests, the steering it received, and the earlier
// questions on this stage so a follow-up makes sense.
func questionPrompt(a engine.Assignment, record *attemptRecord, q workflow.Question) string {
	var p strings.Builder
	node := a.Revision.Config.Workflow.Nodes[q.Node]
	fmt.Fprintf(&p, "A person is asking about the %s stage (%s) of an envctl run.\n\nRun objective:\n%s\n\n", q.Node, node.Kind, a.Revision.Objective)
	fmt.Fprintf(&p, "Attempt %d is %s.\n", a.Attempt.Number, a.Attempt.State)
	result := record.Result
	if a.Attempt.Result != nil {
		result = *a.Attempt.Result
	}
	if result.Summary != "" {
		fmt.Fprintf(&p, "\nWorker's summary:\n%s\n", result.Summary)
	}
	if result.Review.Summary != "" {
		fmt.Fprintf(&p, "\nSupervisor's review:\n%s\n", result.Review.Summary)
	}
	if len(result.Checks) > 0 {
		p.WriteString("\nChecks:\n")
		for _, c := range result.Checks {
			outcome := "passed"
			if !c.Passed {
				outcome = fmt.Sprintf("failed (exit %d)", c.ExitCode)
			}
			fmt.Fprintf(&p, "- %s %s\n", c.Name, outcome)
		}
	}
	if prs := publishedPRs(a, result); len(prs) > 0 {
		p.WriteString("\nPull requests opened for this run:\n")
		for _, line := range prs {
			p.WriteString("- " + line + "\n")
		}
	} else {
		p.WriteString("\nNo pull request has been opened for this run yet.\n")
	}
	var steering []string
	for _, m := range a.Revision.Messages {
		if m.Node == "" || m.Node == q.Node {
			steering = append(steering, "to the "+m.Recipient+": "+m.Body)
		}
	}
	if len(steering) > 0 {
		p.WriteString("\nSteering this stage received:\n- " + strings.Join(steering, "\n- ") + "\n")
	}
	for _, earlier := range a.Revision.Questions {
		if earlier.Node == q.Node && earlier.ID != q.ID && earlier.State == workflow.QuestionAnswered {
			fmt.Fprintf(&p, "\nEarlier question: %s\nYour answer: %s\n", earlier.Text, earlier.Answer)
		}
	}
	fmt.Fprintf(&p, "\nQuestion:\n%s\n\n", q.Text)
	p.WriteString("Answer the question directly and concisely. You may read the worktree to check your answer, but you cannot change anything: this is a question, not a request to redo or alter the work. If the answer is not knowable from the work and the facts above, say so plainly instead of guessing. Return only the answer.")
	return p.String()
}

// publishedPRs lists pull requests from any accepted checkpoint of the run's
// current revision, since a question on an early stage may still be about the
// change the run opened later.
func publishedPRs(a engine.Assignment, result workflow.Result) []string {
	found := map[string]string{}
	for key, url := range result.PRs {
		found[key] = url
	}
	for _, cp := range a.Revision.Checkpoints {
		for key, url := range cp.Result.PRs {
			found[key] = url
		}
	}
	lines := make([]string, 0, len(found))
	for key, url := range found {
		lines = append(lines, key+": "+url)
	}
	sort.Strings(lines)
	return lines
}
