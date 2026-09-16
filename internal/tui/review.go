package tui

import (
	"encoding/json"
	"fmt"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func (m Model) viewRevision() *workflow.Revision {
	r := m.current()
	if r == nil {
		return nil
	}
	if m.ViewedRevision != "" {
		if rev := r.Revision(m.ViewedRevision); rev != nil {
			return rev
		}
	}
	return r.Current()
}
func (m Model) revisionLabel() string {
	if m.current() != nil && m.viewRevision().ID != m.current().CurrentRevision {
		return " · historical view"
	}
	return " · current"
}
func (m Model) viewKey() string {
	r := m.current()
	if r == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s/%s/%d/%d", r.ID, m.viewRevision().ID, m.nodeID(), m.Panel, m.ArtifactIndex)
}
func (m *Model) clearArtifact() {
	m.ArtifactText = ""
	m.ArtifactRequest = ""
	m.DiffRequest = ""
	m.ArtifactIndex = 0
	m.Offset = 0
}
func (m Model) diffKey() string {
	base := ""
	if m.current() != nil && m.CompareRun == m.current().ID {
		base = m.CompareFrom
	}
	key := m.viewKey() + "/" + base
	if cp, ok := m.selectedCheckpoint(); ok {
		key += "/" + cp.ID
	}
	return key
}
func (m *Model) beginInput(mode string) {
	m.Mode = mode
	m.Error, m.refreshFailed = "", false
	if r := m.current(); r != nil {
		m.InputRun = r.ID
		m.InputRevision = m.viewRevision().ID
		m.InputNode = m.nodeID()
	}
}

// approvalStatus explains why nothing can be approved. Approved work that
// has not published yet says so, including a publication failure the
// coordinator is retrying, which a rewind to the stage may be needed to clear.
func approvalStatus(v *workflow.Revision) string {
	a := v.ApprovedUnpublished()
	switch {
	case a == nil:
		return "nothing in this run is awaiting approval"
	case v.Recovery != nil && v.Recovery.Phase == "publication":
		return a.Node + " is already approved, but publishing failed: " + v.Recovery.Detail + ". The coordinator retries; if the failure cannot clear (for example the base branch moved), press r on " + a.Node + " to rewind, then approve again."
	default:
		return a.Node + " is already approved and is publishing"
	}
}

// loadWorkflows offers the repository's workflows to the new-run input. A
// missing or invalid configuration leaves only the default; creating the run
// reports the configuration problem.
func (m *Model) loadWorkflows() {
	m.Workflows, m.NewWorkflow = []string{workflow.DefaultWorkflow}, workflow.DefaultWorkflow
	if c, err := workflow.Load(m.Root); err == nil {
		m.Workflows = c.WorkflowNames()
	}
}
func (m *Model) moveRevision(delta int) {
	r := m.current()
	if r == nil {
		return
	}
	id := m.viewRevision().ID
	for i, rev := range r.Revisions {
		if rev.ID != id {
			continue
		}
		next := max(0, min(len(r.Revisions)-1, i+delta))
		m.ViewedRevision = r.Revisions[next].ID
		if m.ViewedRevision == r.CurrentRevision {
			m.ViewedRevision = ""
		}
		m.Node = min(m.Node, max(0, len(r.Revisions[next].Config.Workflow.Nodes)-1))
		m.clearArtifact()
		return
	}
}
func (m Model) selectedCheckpoint() (workflow.Checkpoint, bool) {
	if rev := m.viewRevision(); rev != nil {
		cp, ok := rev.Checkpoints[m.nodeID()]
		return cp, ok
	}
	return workflow.Checkpoint{}, false
}
func artifactPreview(raw []byte) string {
	var value any
	if json.Unmarshal(raw, &value) == nil {
		var trim func(any) any
		trim = func(v any) any {
			switch x := v.(type) {
			case map[string]any:
				for k, v := range x {
					if k == "screenshot_png" {
						x[k] = "[PNG screenshot retained in this artifact]"
					} else {
						x[k] = trim(v)
					}
				}
			case []any:
				for i, v := range x {
					x[i] = trim(v)
				}
			}
			return v
		}
		raw, _ = json.MarshalIndent(trim(value), "", "  ")
	}
	if len(raw) > 512<<10 {
		raw = append(append([]byte(nil), raw[:512<<10]...), []byte("\n[Preview truncated; complete artifact remains retained.]")...)
	}
	return clean(string(raw))
}
