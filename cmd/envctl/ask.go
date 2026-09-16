package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// answerWait covers the coordinator starting the answer and the answer's own
// time limit, with room for a slow start.
const answerWait = 12 * time.Minute

// runGetter is the part of the daemon client awaitAnswer needs, so a test can
// supply the run's progress without a coordinator.
type runGetter interface {
	Get(ctx context.Context, id string) (*workflow.Run, error)
}

// awaitAnswer prints the answer to the question just asked. Without --wait it
// reports that the question was recorded and returns.
func awaitAnswer(cmd *cobra.Command, g *globals, client runGetter, asked *workflow.Run, req daemon.ActionRequest, wait bool, timeout time.Duration) error {
	id := askedQuestion(asked, req)
	if id == "" {
		return errors.New("the question was accepted but is not on the run; run envctl run show to find it")
	}
	if !wait {
		return printQuestion(cmd, g, asked.Current().Question(id))
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
	defer cancel()
	tick := time.NewTicker(questionPoll)
	defer tick.Stop()
	for {
		run, err := client.Get(ctx, asked.ID)
		if err != nil {
			return err
		}
		if q := findQuestion(run, id); q != nil && q.Finished() {
			return printQuestion(cmd, g, q)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no answer within %s; the question stays open, and envctl run show will show the answer when it arrives", timeout)
		case <-tick.C:
		}
	}
}

// questionPoll is how often awaitAnswer checks for an answer.
var questionPoll = 2 * time.Second

// askedQuestion finds the question this request just added: the latest one on
// the stage with that text, which is the one the action appended.
func askedQuestion(run *workflow.Run, req daemon.ActionRequest) string {
	rev := run.Revision(req.Revision)
	if rev == nil {
		rev = run.Current()
	}
	text := strings.TrimSpace(req.Message)
	for i := len(rev.Questions) - 1; i >= 0; i-- {
		if q := rev.Questions[i]; q.Node == req.Node && q.Text == text {
			return q.ID
		}
	}
	return ""
}

func findQuestion(run *workflow.Run, id string) *workflow.Question {
	for i := range run.Revisions {
		if q := run.Revisions[i].Question(id); q != nil {
			return q
		}
	}
	return nil
}

func printQuestion(cmd *cobra.Command, g *globals, q *workflow.Question) error {
	if q == nil {
		return errors.New("question not found")
	}
	if g.jsonOut {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(q)
	}
	out := cmd.OutOrStdout()
	switch q.State {
	case workflow.QuestionAnswered:
		_, err := fmt.Fprintf(out, "%s's supervisor:\n%s\n", q.Node, q.Answer)
		return err
	case workflow.QuestionFailed:
		return fmt.Errorf("the %s supervisor could not answer: %s", q.Node, q.Detail)
	}
	_, err := fmt.Fprintf(out, "Asked %s's supervisor (%s). envctl run show will show the answer.\n", q.Node, q.ID)
	return err
}

// printQuestions lists questions asked on a revision and their answers, which
// is where `envctl run ask --wait=false` says the answer will appear.
func printQuestions(out io.Writer, rev *workflow.Revision) {
	if len(rev.Questions) == 0 {
		return
	}
	fmt.Fprintln(out, "  questions:")
	for _, q := range rev.Questions {
		fmt.Fprintf(out, "    %s asked %s's supervisor: %s\n", askerName(q.Asker), q.Node, q.Text)
		switch q.State {
		case workflow.QuestionAnswered:
			fmt.Fprintf(out, "      answer: %s\n", strings.ReplaceAll(q.Answer, "\n", "\n              "))
		case workflow.QuestionFailed:
			fmt.Fprintf(out, "      not answered: %s\n", q.Detail)
		default:
			fmt.Fprintf(out, "      waiting for an answer (%s)\n", q.State)
		}
	}
}

func askerName(asker string) string {
	if asker == "" || asker == "local" {
		return "you"
	}
	return asker
}
