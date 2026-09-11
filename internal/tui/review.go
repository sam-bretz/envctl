package tui

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
	if r := m.current(); r != nil {
		m.InputRun = r.ID
		m.InputRevision = m.viewRevision().ID
		m.InputNode = m.nodeID()
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
func checkpointSummary(cp workflow.Checkpoint, selected int) string {
	lines := []string{cp.Node + " · " + cp.ID, cp.Result.Summary, "", "Supervisor: " + cp.Result.Review.Summary}
	if cp.HistoricalOnly {
		lines = append(lines, "Historical only: this output cannot satisfy a new workflow dependency.")
	}
	if cp.Approval != nil {
		lines = append(lines, "Approved by "+cp.Approval.Actor+" at "+cp.Approval.At.Format("2006-01-02 15:04:05 MST"), "Approved work: "+cp.Approval.ResultDigest)
	}
	for _, requirement := range cp.Result.Requirements {
		lines = append(lines, fmt.Sprintf("Requirement %s -> %s: %s", requirement.Capability, strings.Join(requirement.Nodes, ", "), requirement.Reason))
	}
	lines = append(lines, "", "Artifacts (,/. select; o open):")
	for i, a := range cp.Result.Artifacts {
		marker := " "
		if i == selected {
			marker = ">"
		}
		lines = append(lines, fmt.Sprintf("%s %s · %s · %d bytes", marker, a.Name, a.MediaType, a.Size))
	}
	lines = append(lines, "", "Repository commits:")
	keys := make([]string, 0, len(cp.Result.Commits))
	for k := range cp.Result.Commits {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines = append(lines, k+": "+cp.Result.Commits[k])
		parents := make([]string, 0, len(cp.Result.MergeParents[k]))
		for parent := range cp.Result.MergeParents[k] {
			parents = append(parents, parent)
		}
		sort.Strings(parents)
		for _, parent := range parents {
			lines = append(lines, "  Merge input "+parent+": "+cp.Result.MergeParents[k][parent])
		}
		if objects, ok := cp.Result.SourceObjects[k]; ok {
			lines = append(lines, fmt.Sprintf("  Submodule/LFS objects: %s (%d bytes)", objects.Digest, objects.Size))
		}
		if link := cp.Result.PRs[k]; link != "" {
			lines = append(lines, "  PR: "+link)
		}
	}
	if len(cp.Result.Datasets) > 0 {
		lines = append(lines, "", "Datasets: "+cp.Result.DatasetDigest)
		keys = nil
		for k := range cp.Result.Datasets {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			d := cp.Result.Datasets[k]
			lines = append(lines, fmt.Sprintf("%s · %s · %s\n  Snapshot: %s (%d bytes)\n  Verification: %s", k, d.Adapter, d.ToolVersion, d.Source.Digest, d.Source.Size, d.Evidence.Digest))
		}
	}
	return strings.Join(lines, "\n")
}
func (m Model) history() string {
	r := m.current()
	if r == nil {
		return "No run selected."
	}
	lines := []string{"[ previous revision · ] next/current revision", "Viewing history leaves execution running.", ""}
	for _, rev := range r.Revisions {
		marker := " "
		if rev.ID == m.viewRevision().ID {
			marker = ">"
		}
		label := rev.State
		if rev.ID == r.CurrentRevision {
			label += " · current"
		}
		lines = append(lines, fmt.Sprintf("%s %s · %s\n  %s\n  Parent: %s · restored checkpoint: %s", marker, rev.ID, label, rev.Objective, rev.Parent, rev.FromCheckpoint))
		order, _ := rev.Config.Workflow.Order()
		for _, node := range order {
			if cp, ok := rev.Checkpoints[node]; ok {
				kind := "accepted"
				if cp.HistoricalOnly {
					kind = "historical only"
				}
				lines = append(lines, fmt.Sprintf("  %s: %s · %s", node, cp.ID, kind))
			}
		}
	}
	return strings.Join(lines, "\n")
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
