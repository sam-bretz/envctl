package workflow

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// Question states. A question is pending until the coordinator starts
// answering it, and ends answered or failed.
const (
	QuestionPending   = "pending"
	QuestionAnswering = "answering"
	QuestionAnswered  = "answered"
	QuestionFailed    = "failed"
)

// MaxQuestionLength bounds a question, which is placed into an agent prompt.
const MaxQuestionLength = 4000

// Question is something a person asked a stage's supervisor. It is answered
// by a separate, read-only harness invocation, so asking never interrupts or
// alters the work it asks about.
type Question struct {
	ID string `json:"id"`
	// Node and Attempt are the stage and the attempt whose supervisor
	// answers: the latest attempt of that stage when the question was asked.
	Node       string    `json:"node"`
	Attempt    string    `json:"attempt"`
	Text       string    `json:"text"`
	Asker      string    `json:"asker,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	State      string    `json:"state"`
	Answer     string    `json:"answer,omitempty"`
	Detail     string    `json:"detail,omitempty"`
	AnsweredAt time.Time `json:"answered_at,omitzero"`
	// Usage is what answering cost. It counts toward the run's token ceiling
	// and is attributed to the stage through Node.
	Usage *Usage `json:"usage,omitempty"`
}

// Finished reports whether a question has reached a final state.
func (q Question) Finished() bool { return q.State == QuestionAnswered || q.State == QuestionFailed }

// Ask records a question for a stage's supervisor on the current revision.
func (r *Run) Ask(revision, node, text, asker string, now time.Time) (string, error) {
	if !r.Schedulable(revision) {
		return "", errors.New("questions go to the current revision or a variation still being compared; an older one no longer has a running supervisor")
	}
	rev := r.Revision(revision)
	if _, ok := rev.Config.Workflow.Nodes[node]; !ok {
		return "", fmt.Errorf("workflow has no stage %q", node)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("ask a question")
	}
	if len(text) > MaxQuestionLength {
		return "", fmt.Errorf("a question is at most %d characters", MaxQuestionLength)
	}
	// Answering spends tokens, so a run already at its ceiling cannot ask:
	// the answer would go over a limit the person set.
	if exceeded, reason := r.Budget(); exceeded {
		return "", fmt.Errorf("the run has reached its ceiling (%s), and answering would spend more", reason)
	}
	attempt := ""
	for _, a := range rev.Attempts {
		if a.Node == node {
			attempt = a.ID
		}
	}
	if attempt == "" {
		return "", fmt.Errorf("%s has not started, so there is nothing to ask its supervisor about yet", node)
	}
	id := ID("question")
	rev.Questions = append(rev.Questions, Question{ID: id, Node: node, Attempt: attempt, Text: text, Asker: asker, CreatedAt: now, State: QuestionPending})
	return id, nil
}

// Question returns a question by ID.
func (r *Revision) Question(id string) *Question {
	for i := range r.Questions {
		if r.Questions[i].ID == id {
			return &r.Questions[i]
		}
	}
	return nil
}
