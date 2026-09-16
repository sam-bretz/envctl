package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// progress hands back a run whose question moves through the given states,
// one per poll.
type progress struct {
	run    *workflow.Run
	states []workflow.Question
	polls  int
}

func (p *progress) Get(context.Context, string) (*workflow.Run, error) {
	i := min(p.polls, len(p.states)-1)
	p.polls++
	next := *workflow.Clone(p.run)
	next.Current().Questions = []workflow.Question{p.states[i]}
	return &next, nil
}

func askedRun(t *testing.T) (*workflow.Run, daemon.ActionRequest) {
	t.Helper()
	c, err := workflow.Parse([]byte("version: 2\nproject: demo\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := workflow.NewRun("demo", "ship it", "dev", c, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r.Current().Questions = []workflow.Question{{ID: "question_1", Node: "code", Text: "is there a PR?", State: workflow.QuestionPending}}
	return r, daemon.ActionRequest{Revision: r.CurrentRevision, Node: "code", Message: "  is there a PR?  "}
}

func runAsk(t *testing.T, client runGetter, r *workflow.Run, req daemon.ActionRequest, wait bool, timeout time.Duration) (string, error) {
	t.Helper()
	old := questionPoll
	questionPoll = time.Millisecond
	t.Cleanup(func() { questionPoll = old })
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	var out bytes.Buffer
	cmd.SetOut(&out)
	err := awaitAnswer(cmd, &globals{}, client, r, req, wait, timeout)
	return out.String(), err
}

func TestRunAskWaitsForTheAnswerAndPrintsIt(t *testing.T) {
	r, req := askedRun(t)
	p := &progress{run: r, states: []workflow.Question{
		{ID: "question_1", Node: "code", State: workflow.QuestionPending},
		{ID: "question_1", Node: "code", State: workflow.QuestionAnswering},
		{ID: "question_1", Node: "code", State: workflow.QuestionAnswered, Answer: "No PR yet; approved-change opens it."},
	}}
	out, err := runAsk(t, p, r, req, true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "No PR yet; approved-change opens it.") {
		t.Fatalf("answer not printed:\n%s", out)
	}
}

func TestRunAskReportsAnUnanswerableQuestionAsAnError(t *testing.T) {
	r, req := askedRun(t)
	p := &progress{run: r, states: []workflow.Question{{ID: "question_1", Node: "code", State: workflow.QuestionFailed, Detail: "the stage's VM is no longer running"}}}
	if _, err := runAsk(t, p, r, req, true, time.Second); err == nil || !strings.Contains(err.Error(), "VM is no longer running") {
		t.Fatalf("a failed answer was not reported: %v", err)
	}
}

func TestRunAskGivesUpWaitingWithoutLosingTheQuestion(t *testing.T) {
	r, req := askedRun(t)
	p := &progress{run: r, states: []workflow.Question{{ID: "question_1", Node: "code", State: workflow.QuestionAnswering}}}
	_, err := runAsk(t, p, r, req, true, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "stays open") {
		t.Fatalf("a timeout did not say the question is still open: %v", err)
	}
}

func TestRunAskWithoutWaitingSaysWhereTheAnswerWillBe(t *testing.T) {
	r, req := askedRun(t)
	out, err := runAsk(t, &progress{run: r}, r, req, false, time.Second)
	if err != nil || !strings.Contains(out, "envctl run show") {
		t.Fatalf("no-wait did not point to where the answer appears: %v\n%s", err, out)
	}
}

func TestRunShowListsQuestionsAndTheirAnswers(t *testing.T) {
	r, _ := askedRun(t)
	r.Current().Questions = []workflow.Question{
		{Node: "code", Asker: "sam", Text: "is there a PR?", State: workflow.QuestionAnswered, Answer: "not yet"},
		{Node: "qa", Text: "why?", State: workflow.QuestionFailed, Detail: "VM released"},
		{Node: "plan", Text: "scope?", State: workflow.QuestionAnswering},
	}
	var out bytes.Buffer
	printQuestions(&out, r.Current())
	for _, want := range []string{"sam asked code's supervisor: is there a PR?", "answer: not yet", "you asked qa's supervisor: why?", "not answered: VM released", "waiting for an answer (answering)"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q:\n%s", want, out.String())
		}
	}
}
