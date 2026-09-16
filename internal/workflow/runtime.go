package workflow

import (
	"slices"
	"time"
)

// ChildRuntime belongs to one node for its full retry lifetime. Its source,
// stack, data, plugin receipts and harness sessions cannot belong to a sibling.
// The logical revision remains unchanged; this is execution ownership only.
type ChildRuntime struct {
	Runtime            RuntimeState `json:"runtime"`
	Readiness          []Probe      `json:"readiness,omitempty"`
	ReadinessCheckedAt time.Time    `json:"readiness_checked_at,omitempty"`
	Recovery           *Recovery    `json:"recovery,omitempty"`
}

func (s RuntimeState) OccupiesVM() bool {
	return s.Ready || s.State == "preparing" || s.State == "releasing"
}

func (r *Revision) HasChildVMs() bool {
	for _, child := range r.ChildRuntimes {
		if child != nil && child.Runtime.OccupiesVM() {
			return true
		}
	}
	return false
}

// VMCount includes reservations before provisioning and ownership retained
// during release. It must be checked in the same transaction as reservation.
func (r *Run) VMCount() int {
	count := 0
	for _, v := range r.Revisions {
		// An active revision counts even while its runtime reports not ready,
		// since it normally still holds a VM. A parked variation is the
		// exception: it released its VM on purpose so a sibling could start.
		occupies := v.Runtime.OccupiesVM() || slices.Contains([]string{"preparing", "active", "draining"}, v.State)
		if v.Parked() {
			occupies = false
		}
		if occupies {
			count++
		}
		for _, child := range v.ChildRuntimes {
			if child != nil && child.Runtime.OccupiesVM() {
				count++
			}
		}
	}
	return count
}
