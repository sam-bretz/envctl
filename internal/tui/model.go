// Package tui is a Bubble Tea client. It never owns workflow scheduling or
// process lifetimes, so leaving the dashboard only detaches the client.
package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/sam-bretz/envctl/internal/daemon"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/review"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type API interface {
	List(context.Context) ([]workflow.Run, error)
	Create(context.Context, daemon.CreateRequest) (*workflow.Run, error)
	Action(context.Context, string, daemon.ActionRequest) (*workflow.Run, error)
	Artifact(context.Context, string) ([]byte, error)
	Diff(context.Context, string, review.Request) (review.Comparison, error)
}
type Model struct {
	API             API
	Root            string
	Runs            []workflow.Run
	Selected        int
	Node            int
	Panel           int
	Width           int
	Height          int
	Offset          int
	Recipient       string
	Input           string
	Mode            string
	Error           string
	Notice          string
	ArtifactText    string
	ViewedRevision  string
	ArtifactIndex   int
	ArtifactRequest string
	InputRun        string
	InputRevision   string
	InputNode       string
	CompareRun      string
	CompareFrom     string
	DiffRequest     string
}
type snapshotMsg struct {
	runs []workflow.Run
	err  error
}
type actionMsg struct {
	run *workflow.Run
	err error
}
type artifactMsg struct {
	key  string
	body []byte
	err  error
}
type diffMsg struct {
	key        string
	comparison review.Comparison
	err        error
}
type tickMsg time.Time

var panels = []string{"Conversation", "Checkpoint", "Changes", "Tests", "Services", "Readiness", "Graph", "History"}

func New(api API, root string) Model {
	return Model{API: api, Root: root, Width: 100, Height: 30, Recipient: "supervisor"}
}
func (m Model) Init() tea.Cmd { return tea.Batch(m.refresh(), tick()) }
func tick() tea.Cmd           { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }
func (m Model) refresh() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		runs, err := m.API.List(ctx)
		return snapshotMsg{runs, err}
	}
}
func (m Model) current() *workflow.Run {
	if m.Selected < 0 || m.Selected >= len(m.Runs) {
		return nil
	}
	return &m.Runs[m.Selected]
}
func (m Model) nodeID() string {
	r := m.current()
	if r == nil {
		return ""
	}
	order, _ := m.viewRevision().Config.Workflow.Order()
	if len(order) == 0 {
		return ""
	}
	return order[min(m.Node, len(order)-1)]
}
func (m Model) act(req daemon.ActionRequest) tea.Cmd {
	r := m.current()
	if r == nil {
		return nil
	}
	if m.viewRevision().ID != r.CurrentRevision {
		return func() tea.Msg {
			return actionMsg{err: fmt.Errorf("history is read-only; press ] to return to the current revision")}
		}
	}
	req.OperationID = workflow.ID("op")
	req.ExpectedVersion = r.Version
	req.Revision = r.CurrentRevision
	id := r.ID
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		r, err := m.API.Action(ctx, id, req)
		return actionMsg{r, err}
	}
}
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch v := msg.(type) {
	case tea.WindowSizeMsg:
		m.Width = max(10, v.Width)
		m.Height = max(5, v.Height)
	case snapshotMsg:
		if v.err != nil {
			m.Error = v.err.Error()
			return m, nil
		}
		previousView := m.viewKey()
		id := ""
		if r := m.current(); r != nil {
			id = r.ID
		}
		m.Runs = v.runs
		m.Selected = min(m.Selected, max(0, len(m.Runs)-1))
		for i, r := range m.Runs {
			if r.ID == id {
				m.Selected = i
			}
		}
		if m.viewKey() != previousView {
			m.clearArtifact()
		}
		m.Error = ""
	case actionMsg:
		if v.err != nil {
			m.Error = v.err.Error()
		} else {
			m.Notice = "Saved"
			m.Error = ""
		}
		return m, m.refresh()
	case artifactMsg:
		if v.key != m.ArtifactRequest || v.key != m.viewKey() {
			return m, nil
		}
		if v.err != nil {
			m.Error = v.err.Error()
		} else {
			m.ArtifactText = artifactPreview(v.body)
			m.Offset = 0
		}
	case diffMsg:
		if v.key != m.DiffRequest || v.key != m.diffKey() {
			return m, nil
		}
		if v.err != nil {
			m.Error = v.err.Error()
		} else {
			m.ArtifactText = review.Text(v.comparison)
			m.Offset = 0
		}
	case tickMsg:
		return m, tea.Batch(m.refresh(), tick())
	case tea.PasteMsg:
		if m.Mode != "" {
			m.Input += clean(v.Content)
		}
	case tea.MouseClickMsg:
		mouse := v.Mouse()
		if m.Width >= 70 && mouse.Y >= 5 && mouse.Y < 5+m.listHeight() {
			i := max(0, m.Selected-m.listHeight()+1) + mouse.Y - 5
			if i < len(m.Runs) {
				m.Selected = i
				m.ViewedRevision = ""
				m.Node = 0
				m.Offset = 0
				m.clearArtifact()
			}
		}
	case tea.KeyPressMsg:
		key := v.String()
		if key == "ctrl+c" {
			return m, tea.Quit
		}
		if m.Mode != "" {
			switch key {
			case "esc":
				m.Mode = ""
				m.Input = ""
			case "backspace":
				runes := []rune(m.Input)
				if len(runes) > 0 {
					m.Input = string(runes[:len(runes)-1])
				}
			case "enter":
				mode, body := m.Mode, strings.TrimSpace(m.Input)
				if mode != "new" && (m.current() == nil || m.current().ID != m.InputRun || m.current().CurrentRevision != m.InputRevision || m.nodeID() != m.InputNode) {
					m.Mode = ""
					m.Input = ""
					m.Error = "The selected run, revision or stage changed; reopen the input before sending."
					return m, nil
				}
				if body == "" {
					return m, nil
				}
				m.Mode = ""
				m.Input = ""
				if mode == "chat" {
					return m, m.act(daemon.ActionRequest{Action: "message", Node: m.nodeID(), Recipient: m.Recipient, Message: body})
				}
				if mode == "rewind" {
					return m, m.act(daemon.ActionRequest{Action: "rewind", Node: m.nodeID(), Task: body})
				}
				if mode == "plugin file (reopens Plan)" {
					ref, err := plugin.ReadRef(body)
					if err != nil {
						m.Error = err.Error()
						return m, nil
					}
					return m, m.act(daemon.ActionRequest{Action: "plugin-attach", Plugin: &ref})
				}
				if mode == "remove plugin ID (reopens Plan)" {
					return m, m.act(daemon.ActionRequest{Action: "plugin-remove", PluginID: body})
				}
				if mode == "new" {
					api, root := m.API, m.Root
					return m, func() tea.Msg {
						c, err := workflow.Load(root)
						if err != nil {
							return actionMsg{err: err}
						}
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						defer cancel()
						r, err := api.Create(ctx, daemon.CreateRequest{OperationID: workflow.ID("op"), Name: body, Task: body, Owner: "local", Config: c})
						return actionMsg{r, err}
					}
				}
			default:
				m.Input += clean(v.Key().Text)
			}
			return m, nil
		}
		switch key {
		case "esc":
			m.clearArtifact()
		case "q":
			return m, tea.Quit
		case "j", "down":
			m.Selected = min(m.Selected+1, max(0, len(m.Runs)-1))
			m.ViewedRevision = ""
			m.Node = 0
			m.Offset = 0
			m.clearArtifact()
		case "k", "up":
			m.Selected = max(0, m.Selected-1)
			m.ViewedRevision = ""
			m.Node = 0
			m.Offset = 0
			m.clearArtifact()
		case "right", "l":
			if r := m.current(); r != nil {
				m.Node = min(m.Node+1, len(m.viewRevision().Config.Workflow.Nodes)-1)
			}
			m.Offset = 0
			m.clearArtifact()
		case "left", "h":
			m.Node = max(0, m.Node-1)
			m.Offset = 0
			m.clearArtifact()
		case "tab":
			m.Panel = (m.Panel + 1) % len(panels)
			m.Offset = 0
			m.clearArtifact()
		case "shift+tab":
			m.Panel = (m.Panel + len(panels) - 1) % len(panels)
			m.Offset = 0
			m.clearArtifact()
		case "pgdown":
			m.Offset += max(1, m.Height/3)
		case "pgup":
			m.Offset = max(0, m.Offset-max(1, m.Height/3))
		case "i":
			m.beginInput("chat")
		case "n":
			m.beginInput("new")
		case "p":
			m.beginInput("plugin file (reopens Plan)")
		case "P":
			m.beginInput("remove plugin ID (reopens Plan)")
		case "r":
			if r := m.current(); r != nil {
				m.beginInput("rewind")
				m.Input = m.viewRevision().Objective
			}
		case "s":
			if m.Recipient == "supervisor" {
				m.Recipient = "worker"
			} else {
				m.Recipient = "supervisor"
			}
		case "x":
			return m, m.act(daemon.ActionRequest{Action: "cancel"})
		case "a":
			if r := m.current(); r != nil {
				for _, a := range r.Current().Attempts {
					if a.Node == m.nodeID() && a.State == "awaiting-approval" && a.Result != nil {
						return m, m.act(daemon.ActionRequest{Action: "approve", Attempt: a.ID, Actor: "local", WorkDigest: a.Result.WorkDigest()})
					}
				}
			}
		case "[":
			m.moveRevision(-1)
		case "]":
			m.moveRevision(1)
		case ",", ".":
			if cp, ok := m.selectedCheckpoint(); ok && len(cp.Result.Artifacts) > 0 {
				step := 1
				if key == "," {
					step = -1
				}
				m.ArtifactIndex = (m.ArtifactIndex + len(cp.Result.Artifacts) + step) % len(cp.Result.Artifacts)
				m.ArtifactText = ""
				m.ArtifactRequest = ""
				m.DiffRequest = ""
				m.Offset = 0
			}
		case "o":
			if cp, ok := m.selectedCheckpoint(); ok && len(cp.Result.Artifacts) > 0 {
				m.DiffRequest = ""
				artifact := cp.Result.Artifacts[min(m.ArtifactIndex, len(cp.Result.Artifacts)-1)]
				m.ArtifactRequest = m.viewKey()
				api, digest, request := m.API, artifact.Digest, m.ArtifactRequest
				return m, func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
					defer cancel()
					raw, err := api.Artifact(ctx, digest)
					return artifactMsg{key: request, body: raw, err: err}
				}
			}
		case "b":
			if cp, ok := m.selectedCheckpoint(); ok {
				m.CompareRun, m.CompareFrom = m.current().ID, cp.ID
				m.clearArtifact()
				m.Notice = "Comparison base: " + cp.Node + " · " + cp.ID + "; select another checkpoint and press d"
			}
		case "B":
			m.CompareRun, m.CompareFrom = "", ""
			m.clearArtifact()
			m.Notice = "Comparison base: revision source pins"
		case "d":
			if _, ok := m.selectedCheckpoint(); ok {
				m.Panel = 2
				m.clearArtifact()
				m.DiffRequest = m.diffKey()
				api, id, request := m.API, m.current().ID, m.DiffRequest
				req := review.Request{Revision: m.viewRevision().ID, Node: m.nodeID()}
				if m.CompareRun == id {
					req.From = m.CompareFrom
				}
				return m, func() tea.Msg {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					comparison, err := api.Diff(ctx, id, req)
					return diffMsg{key: request, comparison: comparison, err: err}
				}
			}
		}
	}
	return m, nil
}
func (m Model) listHeight() int {
	if m.Width < 70 || m.Height < 18 {
		return 0
	}
	return min(len(m.Runs), max(1, min(5, m.Height/5)))
}
func status(rev *workflow.Revision, node string) string {
	if cp, ok := rev.Checkpoints[node]; ok {
		if cp.HistoricalOnly {
			return "historical"
		}
		return "done"
	}
	if child := rev.ChildRuntimes[node]; child != nil && child.Runtime.OccupiesVM() && child.Recovery != nil {
		return "recovering"
	}
	for i := len(rev.Attempts) - 1; i >= 0; i-- {
		a := rev.Attempts[i]
		if a.Node == node {
			return a.State
		}
	}
	return "pending"
}
func (m Model) View() tea.View {
	width, height := max(10, m.Width), max(5, m.Height)
	compact := width < 70 || height < 18
	lines := []string{"envctl  /  local workflows", ""}
	if r := m.current(); r != nil {
		rev := m.viewRevision()
		order, _ := rev.Config.Workflow.Order()
		stages := []string{}
		for i, id := range order {
			label := id + " " + status(rev, id)
			if i == m.Node {
				label = "[" + label + "]"
			}
			stages = append(stages, label)
		}
		lines[1] = strings.Join(stages, " → ")
	} else {
		lines[1] = "No workflow runs yet. Press n to start from this repository."
	}
	lines = append(lines, "", "IN-FLIGHT WORKFLOWS", "")
	if m.listHeight() > 0 {
		start := max(0, m.Selected-m.listHeight()+1)
		for i := start; i < min(len(m.Runs), start+m.listHeight()); i++ {
			r := m.Runs[i]
			marker := " "
			if i == m.Selected {
				marker = ">"
			}
			branch := ""
			if len(r.Current().Config.Repositories) > 0 {
				branch = workflow.OutputBranch(r.Current().Config.Repositories[0], r.CurrentRevision)
			}
			lines = append(lines, fmt.Sprintf("%s %-24s %-12s %-8s %s", marker, r.Name, r.Current().State, "local", branch))
		}
	}
	if r := m.current(); r != nil {
		rev := m.viewRevision()
		lines = append(lines, "", r.Name+" · "+rev.State+" · "+rev.ID+m.revisionLabel(), rev.Objective)
		if rev.Recovery != nil {
			lines = append(lines, "Recovery ("+rev.Recovery.Phase+"): "+rev.Recovery.Detail)
		}
		if rev.Runtime.PreviewURL != "" {
			lines = append(lines, "Preview: "+rev.Runtime.PreviewURL)
		}
	}
	labels := []string{}
	for i, p := range panels {
		if i == m.Panel {
			p = "[" + p + "]"
		}
		labels = append(labels, p)
	}
	lines = append(lines, "", strings.Join(labels, "  "), strings.Repeat("─", width))
	if compact {
		title := "envctl · " + panels[m.Panel]
		if r := m.current(); r != nil {
			title = r.Name + " · " + m.nodeID() + " · " + panels[m.Panel] + m.revisionLabel()
		}
		lines = []string{title}
	}
	detail := strings.Split(clean(m.details()), "\n")
	room := max(0, height-len(lines)-4)
	if compact {
		room = max(0, height-len(lines)-3)
	}
	offset := min(m.Offset, max(0, len(detail)-room))
	for i := 0; i < room; i++ {
		if offset+i < len(detail) {
			lines = append(lines, detail[offset+i])
		} else {
			lines = append(lines, "")
		}
	}
	info := m.Notice
	if m.Error != "" {
		info = "Error: " + m.Error
	}
	lines = append(lines, info)
	prompt := "To " + m.Recipient + " · i chat · n new · r rewind · a approve · o artifact · d diff · p/P plugins · x cancel"
	if compact {
		prompt = "i chat · o artifact"
	}
	if m.Mode != "" {
		prompt = m.Mode + " > " + m.Input
	}
	navigation := "↑/↓ runs  ←/→ stages  tab views  pgup/pgdn scroll  s recipient  [/] history  ,/. artifacts  q detach"
	if compact {
		navigation = "tab views · q detach"
	}
	lines = append(lines, prompt, navigation)
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(clean(lines[i]), width, "")
	}
	view := tea.NewView(strings.Join(lines, "\n"))
	view.AltScreen = true
	view.WindowTitle = "envctl workflows"
	view.MouseMode = tea.MouseModeCellMotion
	return view
}
func (m Model) details() string {
	if m.ArtifactText != "" {
		return m.ArtifactText
	}
	r := m.current()
	if r == nil {
		return "Create a task with n, or use envctl run create --task ..."
	}
	rev := m.viewRevision()
	node := m.nodeID()
	switch panels[m.Panel] {
	case "Conversation":
		lines := []string{}
		for _, a := range rev.Attempts {
			if a.Node == node {
				lines = append(lines, fmt.Sprintf("Worker attempt %d: %s", a.Number, a.State))
				if a.Error != "" {
					lines = append(lines, a.Error)
				}
				if a.Result != nil {
					lines = append(lines, a.Result.Summary, "Supervisor: "+a.Result.Review.Summary)
				}
			}
		}
		for _, msg := range rev.Messages {
			if msg.Node == "" || msg.Node == node {
				lines = append(lines, "To "+msg.Recipient+": "+msg.Body)
			}
		}
		if len(lines) == 0 {
			return "No stage messages yet. Press i to address the " + m.Recipient + "."
		}
		return strings.Join(lines, "\n\n")
	case "Readiness":
		if child := rev.ChildRuntimes[node]; child != nil {
			return "Branch runtime for " + node + ":\n" + pretty(child) + "\n\nParent Plan requirements: " + strings.Join(rev.Requirements(), ", ")
		}
		problems := rev.ReadinessProblems(time.Now(), rev.Requirements())
		var attachments []string
		for _, p := range rev.Config.Plugins {
			attachments = append(attachments, fmt.Sprintf("%s@%s  %s\nCapabilities: %s", p.ID, p.Version, p.Digest, strings.Join(p.Provides, ", ")))
		}
		pluginText := "\n\nInvocation plugins (p add/replace, P remove; reopens Plan):\n" + strings.Join(attachments, "\n")
		if len(rev.DiscoveredRequirements) > 0 {
			pluginText += "\n\nDiscovered during Plan:\n"
			for _, requirement := range rev.DiscoveredRequirements {
				pluginText += fmt.Sprintf("%s -> %s: %s\n", requirement.Capability, strings.Join(requirement.Nodes, ", "), requirement.Reason)
			}
		}
		if rev.Recovery != nil {
			problems = append(problems, fmt.Sprintf("%s: %s\nEvidence: %s\nRetry after: %s", rev.Recovery.Phase, rev.Recovery.Detail, rev.Recovery.EvidenceDigest, rev.Recovery.RetryAt.Format(time.RFC3339)))
		}
		if len(problems) == 0 {
			return "All declared capabilities have current readiness evidence." + pluginText
		}
		return "Plan must resolve these before downstream execution:\n\n" + strings.Join(problems, "\n") + pluginText
	case "Services":
		if child := rev.ChildRuntimes[node]; child != nil {
			return "Branch runtime for " + node + ":\n" + pretty(child.Runtime)
		}
		return pretty(rev.Runtime)
	case "Graph":
		order, _ := rev.Config.Workflow.Order()
		lines := []string{}
		for _, id := range order {
			n := rev.Config.Workflow.Nodes[id]
			lines = append(lines, fmt.Sprintf("%s [%s] <- %s", id, status(rev, id), strings.Join(n.Needs, ", ")))
		}
		return strings.Join(lines, "\n")
	case "Checkpoint":
		if cp, ok := rev.Checkpoints[node]; ok {
			return checkpointSummary(cp, m.ArtifactIndex)
		}
		for _, a := range rev.Attempts {
			if a.Node == node && a.State == "awaiting-approval" {
				return "Awaiting approval of this exact result (a to approve):\n" + pretty(a.Result)
			}
		}
		return "No accepted checkpoint for " + node + "."
	case "History":
		return m.history()
	case "Changes":
		base := "revision source pins"
		if m.CompareRun == r.ID && m.CompareFrom != "" {
			base = m.CompareFrom
		}
		lines := []string{"d load source diff · b use selected checkpoint as base · B reset base", "Compare from: " + base, ""}
		for _, repo := range rev.Config.Repositories {
			lines = append(lines, repo.ID, "  Source pin: "+rev.SourcePins[repo.ID], "  Output branch: "+workflow.OutputBranch(repo, rev.ID))
			if cp, ok := rev.Checkpoints[node]; ok {
				lines = append(lines, "  Checkpoint: "+cp.Result.Commits[repo.ID])
				if link := cp.Result.PRs[repo.ID]; link != "" {
					lines = append(lines, "  PR: "+link)
				}
			}
			if repo.Publication != nil {
				lines = append(lines, "  Destination: "+repo.Publication.Repository+" -> "+repo.Publication.Base)
			}
		}
		return strings.Join(lines, "\n")
	case "Tests":
		if cp, ok := rev.Checkpoints[node]; ok {
			return pretty(cp.Result.Checks)
		}
		return "No test evidence at this checkpoint."
	}
	return ""
}
func pretty(v any) string {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err.Error()
	}
	return string(b)
}
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || !unicode.IsControl(r) {
			return r
		}
		return -1
	}, ansi.Strip(s))
}
func Run(ctx context.Context, api API, root string) error {
	_, err := tea.NewProgram(New(api, root), tea.WithContext(ctx)).Run()
	return err
}
