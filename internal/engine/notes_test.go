package engine

import (
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func notesOfKind(v *workflow.Revision, kind string) []workflow.Note {
	var out []workflow.Note
	for _, n := range v.Notes {
		if n.Kind == kind {
			out = append(out, n)
		}
	}
	return out
}

func TestPublishingRecordsThePullRequestItOpened(t *testing.T) {
	h := setup(t)
	h.until(waitingForApproval)
	r := h.run()
	v := r.Current()
	a := v.Attempts[len(v.Attempts)-1]
	h.mutate(func(r *workflow.Run) error { return r.Approve(h.rev, a.ID, "developer", a.Result.WorkDigest(), h.now) })
	h.until(func(v *workflow.Revision) bool { return v.State == "completed" })
	for i := 0; i < 3; i++ {
		h.step()
	}
	notes := notesOfKind(h.run().Current(), workflow.NotePublished)
	if len(notes) != 1 || !strings.Contains(notes[0].Detail, "https://example.test/pr/1") {
		t.Fatalf("publication not recorded exactly once with its URL: %+v", notes)
	}
}

func TestStoppingAtTheTokenCeilingIsRecordedOnce(t *testing.T) {
	h := setup(t)
	h.until(func(v *workflow.Revision) bool { return len(v.Checkpoints) >= 1 })
	h.mutate(func(r *workflow.Run) error {
		r.Current().Config.Limits.RunTokens = 1
		r.Current().Attempts[0].Usage = &workflow.Usage{Input: 1000}
		return nil
	})
	h.until(func(v *workflow.Revision) bool { return v.State == "needs-attention" })
	// The engine reaches this decision on every tick while the run is stuck.
	for i := 0; i < 5; i++ {
		h.step()
	}
	notes := notesOfKind(h.run().Current(), workflow.NoteTokenCeiling)
	if len(notes) != 1 || !strings.Contains(notes[0].Detail, "token ceiling") {
		t.Fatalf("ceiling stop not recorded exactly once: %+v", notes)
	}
}

func TestANudgeReportedOnEveryPollIsRecordedOnce(t *testing.T) {
	h := setup(t)
	h.backend.startRunning = true
	h.until(func(v *workflow.Revision) bool {
		for _, a := range v.Attempts {
			if a.State == "running" {
				return true
			}
		}
		return false
	})
	v := h.run().Current()
	running := v.Attempts[len(v.Attempts)-1]
	nudge := workflow.Note{At: h.now, Kind: workflow.NoteStallNudge, Node: running.Node, Detail: "nudged the worker after a silence (nudge 1 of 2)"}
	obs := h.backend.observations[running.ID]
	obs.Notes = []workflow.Note{nudge}
	h.backend.observations[running.ID] = obs
	for i := 0; i < 5; i++ {
		h.step()
	}
	if got := notesOfKind(h.run().Current(), workflow.NoteStallNudge); len(got) != 1 {
		t.Fatalf("a nudge reported on every poll was recorded %d times: %+v", len(got), got)
	}
}
