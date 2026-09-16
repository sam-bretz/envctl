package workflow

import "time"

// Note kinds.
const (
	NoteTokenCeiling  = "token-ceiling"
	NoteAttemptBudget = "attempt-budget"
	NoteStallNudge    = "stall-nudge"
	NotePublished     = "published"
	NotePublishFailed = "publish-failed"
	NoteChosen        = "variation-chosen"
)

// Note records a coordinator decision that no other run state keeps. Recovery
// holds only the latest cause and is cleared when a runtime is released, so a
// run that stopped at its token ceiling, nudged a stalled agent or failed to
// publish would otherwise lose that history. The Decision Log reads these.
type Note struct {
	At     time.Time `json:"at"`
	Kind   string    `json:"kind"`
	Node   string    `json:"node,omitempty"`
	Detail string    `json:"detail"`
}

// AddNote appends a note unless an identical one is already recorded, so a
// decision reported on every poll is written once.
func (r *Revision) AddNote(note Note) bool {
	for _, n := range r.Notes {
		if n.Kind == note.Kind && n.Node == note.Node && n.Detail == note.Detail && n.At.Equal(note.At) {
			return false
		}
	}
	r.Notes = append(r.Notes, note)
	return true
}
