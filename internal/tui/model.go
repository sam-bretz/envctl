// Package tui is a Bubble Tea client. It never owns workflow scheduling or
// process lifetimes, so leaving the dashboard only detaches the client.
package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// Workflows lists the workflows the new-run input can select; tab
	// cycles NewWorkflow through them.
	Workflows   []string
	NewWorkflow string
	CompareRun      string
	CompareFrom     string
	DiffRequest     string
	// Theme is empty until the terminal reports its capabilities, so piped and
	// non-interactive rendering stays free of escape sequences.
	Theme theme
	Frame int
	// ConfigPath receives theme choices saved from the picker; empty skips saving.
	ConfigPath string
	Picker     *themePicker
	// OpenURL opens a link in the user's browser; tests substitute it.
	OpenURL func(string) error
	// refreshFailed marks Error as a refresh failure, the only kind a later
	// successful refresh may clear. Action errors stay until the next action.
	refreshFailed bool
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
type openedMsg struct {
	urls  int
	first string
	err   error
}

var panels = []string{"Conversation", "Checkpoint", "Changes", "Tests", "Services", "Readiness", "Graph", "History"}

func New(api API, root string) Model {
	m := Model{API: api, Root: root, Width: 100, Height: 30, Recipient: "supervisor"}
	m.Theme.resolve()
	return m
}
func (m Model) Init() tea.Cmd {
	return tea.Batch(m.refresh(), tick(), tea.RequestBackgroundColor)
}
func tick() tea.Cmd { return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) }) }
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
			m.refreshFailed = true
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
		if m.refreshFailed {
			m.Error = ""
			m.refreshFailed = false
		}
	case actionMsg:
		m.refreshFailed = false
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
	case tea.ColorProfileMsg:
		m.Theme.profile = v.Profile
	case tea.BackgroundColorMsg:
		m.Theme.dark = v.IsDark()
		m.Theme.resolve()
	case openedMsg:
		if v.err != nil {
			m.Error = "could not open the pull request: " + v.err.Error()
		} else if v.urls == 1 {
			m.Error, m.Notice = "", "Opened "+v.first
		} else {
			m.Error, m.Notice = "", fmt.Sprintf("Opened %d pull requests", v.urls)
		}
	case tickMsg:
		m.Frame++
		return m, tea.Batch(m.refresh(), tick())
	case tea.PasteMsg:
		if m.Mode != "" {
			m.Input += clean(v.Content)
		}
	case tea.MouseClickMsg:
		mouse := v.Mouse()
		if m.listHeight() > 0 {
			if rows := m.headerRows(); mouse.Y >= 0 && mouse.Y < len(rows) && rows[mouse.Y].run > 0 {
				m.Selected = rows[mouse.Y].run - 1
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
		if m.Picker != nil {
			return m.updatePicker(key), nil
		}
		if m.Mode != "" {
			switch key {
			case "esc":
				m.Mode = ""
				m.Input = ""
			case "tab", "shift+tab":
				if m.Mode == "new" && len(m.Workflows) > 1 {
					step := 1
					if key == "shift+tab" {
						step = len(m.Workflows) - 1
					}
					m.NewWorkflow = m.Workflows[(slices.Index(m.Workflows, m.NewWorkflow)+step)%len(m.Workflows)]
				}
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
					api, root, selected := m.API, m.Root, m.NewWorkflow
					return m, func() tea.Msg {
						c, err := workflow.Load(root)
						if errors.Is(err, os.ErrNotExist) {
							return actionMsg{err: fmt.Errorf("no envctl.yaml in %s; add a version 2 workflow configuration (see https://sam-bretz.github.io/envctl/first-workflow/)", root)}
						}
						if err == nil {
							c, err = c.SelectWorkflow(selected)
						}
						if err != nil {
							return actionMsg{err: fmt.Errorf("workflow configuration: %w", err)}
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
		case "T":
			m.openPicker()
		case "g":
			r := m.current()
			if r == nil {
				break
			}
			prs := r.PullRequests()
			if len(prs) == 0 {
				m.Notice = ""
				m.Error = "no pull request yet; the approved change opens one after approval"
				break
			}
			open := m.OpenURL
			if open == nil {
				open = OpenInBrowser
			}
			return m, func() tea.Msg {
				for _, pr := range prs {
					if err := open(pr.URL); err != nil {
						return openedMsg{err: err}
					}
				}
				return openedMsg{urls: len(prs), first: prs[0].URL}
			}
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
			m.loadWorkflows()
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
				waiting := ""
				for _, a := range r.Current().Attempts {
					if a.State != "awaiting-approval" || a.Result == nil {
						continue
					}
					if a.Node == m.nodeID() {
						return m, m.act(daemon.ActionRequest{Action: "approve", Attempt: a.ID, Actor: "local", WorkDigest: a.Result.WorkDigest()})
					}
					waiting = a.Node
				}
				// Never approve work the reviewer is not looking at: move to the
				// stage awaiting approval and ask for a second press there.
				if waiting != "" {
					order, _ := m.viewRevision().Config.Workflow.Order()
					for i, id := range order {
						if id == waiting {
							m.Node, m.Offset = i, 0
							m.clearArtifact()
						}
					}
					m.Error = ""
					m.Notice = waiting + " is awaiting approval; review it and press a again to approve"
				} else {
					m.Notice = ""
					m.Error = "nothing in this run is awaiting approval"
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

// chrome is one rendered line of the frame around the detail panel. Optional
// lines are dropped first when the terminal cannot fit the whole frame.
type chrome struct {
	text     string
	optional bool
	run      int // run index + 1 for a run-list row, so clicks map to what was drawn
}

// trim drops optional rows, last first, until rows fit budget.
func trim(rows []chrome, budget int) []chrome {
	rows = append([]chrome(nil), rows...)
	for i := len(rows) - 1; i >= 0 && len(rows) > budget; i-- {
		if rows[i].optional {
			rows = append(rows[:i], rows[i+1:]...)
		}
	}
	if len(rows) > budget {
		rows = rows[:max(0, budget)]
	}
	return rows
}

func fit(rows []chrome, budget int) []string {
	lines := []string{}
	for _, row := range trim(rows, budget) {
		lines = append(lines, row.text)
	}
	return lines
}

// headerRows is the header exactly as View draws it on a full-size terminal.
func (m Model) headerRows() []chrome {
	width, height := max(10, m.Width), max(5, m.Height)
	return trim(m.header(width), max(1, height-len(m.footer(width, false))-3))
}

// stageStrip draws the DAG as checkpoint glyphs joined by connectors, with the
// selected stage bracketed so it reads without color.
func (m Model) stageStrip(width int) string {
	rev := m.viewRevision()
	if rev == nil {
		return m.Theme.dim().Render("No workflow runs yet. Press n to start from this repository.")
	}
	t := m.Theme
	order, _ := rev.Config.Workflow.Order()
	stages := make([]string, 0, len(order))
	for i, id := range order {
		state := status(rev, id)
		glyph, c := t.stageLook(state, m.Frame)
		label := clean(id)
		if i == m.Node {
			stages = append(stages, t.fg(c).Render(glyph)+" "+t.highlight(t.strong(t.accent()).Render("["+label+" "+state+"]")))
			continue
		}
		stages = append(stages, t.fg(c).Render(glyph)+" "+t.dim().Render(label))
	}
	strip := strings.Join(stages, t.fg(t.line()).Render(" ──▸ "))
	if ansi.StringWidth(strip) <= width {
		return strip
	}
	// Too narrow for every label: keep glyphs and name only the selected stage.
	compactStages := make([]string, 0, len(order))
	for i, id := range order {
		glyph, c := t.stageLook(status(rev, id), m.Frame)
		if i == m.Node {
			compactStages = append(compactStages, t.fg(c).Render(glyph)+t.strong(t.accent()).Render(" "+clean(id)))
			continue
		}
		compactStages = append(compactStages, t.fg(c).Render(glyph))
	}
	return ansi.Truncate(strings.Join(compactStages, t.fg(t.line()).Render("─")), width, "…")
}

func (m Model) runRow(i int, width int) string {
	t := m.Theme
	r := m.Runs[i]
	state := r.Current().State
	branch := ""
	if len(r.Current().Config.Repositories) > 0 {
		branch = workflow.OutputBranch(r.Current().Config.Repositories[0], r.CurrentRevision)
	}
	_, c := t.stageLook(state, m.Frame)
	name, badge := t.style(), t.fg(c)
	marker := "  "
	if i == m.Selected {
		marker = t.strong(t.accent()).Render("▸") + " "
		name = t.selected()
	}
	tokens := t.dim()
	if r.UsageFraction() >= 0.8 {
		tokens = t.fg(t.warn())
	}
	row := marker + name.Render(pad(clean(r.Name), 24)) + " " + badge.Render(pad(state, 12)) + " " +
		tokens.Render(pad(workflow.FormatTokens(r.Usage().Tokens()), 7)) + " " + t.dim().Render(pad("local", 6)+" "+clean(branch))
	row = ansi.Truncate(row, width, "…")
	if i == m.Selected {
		row = t.highlight(pad(row, width))
	}
	return row
}

// tabs renders the panel selector. The active panel keeps its brackets so the
// selection survives a monochrome terminal.
func (m Model) tabs(width int) string {
	t := m.Theme
	labels := make([]string, 0, len(panels))
	for i, p := range panels {
		if i == m.Panel {
			style := t.strong(t.accent())
			if t.colored() {
				style = style.Underline(true)
			}
			labels = append(labels, t.highlight(style.Render("["+p+"]")))
			continue
		}
		labels = append(labels, t.dim().Render(p))
	}
	return ansi.Truncate(strings.Join(labels, " "), width, "…")
}

func (m Model) header(width int) []chrome {
	t := m.Theme
	mark := t.strong(t.accent()).Render("▎envctl")
	count := fmt.Sprintf("%d runs", len(m.Runs))
	root := clean(m.Root)
	right := t.dim().Render(count + " · " + root)
	gap := width - ansi.StringWidth(mark) - ansi.StringWidth(right) - 1
	title := mark + " " + t.dim().Render("local workflows")
	if gap > ansi.StringWidth(" local workflows") {
		title = pad(title, width-ansi.StringWidth(right)) + right
	}
	rows := []chrome{{text: ansi.Truncate(title, width, "")}}
	for _, line := range t.box("stages", []string{m.stageStrip(max(0, width-2))}, width) {
		rows = append(rows, chrome{text: line})
	}
	rows = append(rows, chrome{text: "", optional: true})
	if m.listHeight() > 0 {
		rows = append(rows, chrome{text: t.dim().Render("IN-FLIGHT WORKFLOWS")})
		start := max(0, m.Selected-m.listHeight()+1)
		for i := start; i < min(len(m.Runs), start+m.listHeight()); i++ {
			rows = append(rows, chrome{text: m.runRow(i, width), optional: i != m.Selected, run: i + 1})
		}
	}
	if r := m.current(); r != nil {
		rev := m.viewRevision()
		summary := t.bold().Render(clean(r.Name)) + t.dim().Render(" · "+rev.State+" · "+rev.ID+m.revisionLabel())
		if name := rev.Config.WorkflowName; name != "" {
			summary += t.dim().Render(" · " + clean(name) + " workflow")
		}
		usage := t.dim()
		switch fraction := r.UsageFraction(); {
		case fraction >= 1:
			usage = t.fg(t.danger())
		case fraction >= 0.8:
			usage = t.fg(t.warn())
		}
		rows = append(rows,
			chrome{text: "", optional: true},
			chrome{text: ansi.Truncate(summary, width, "…")},
			chrome{text: ansi.Truncate(usage.Render(r.UsageSummary()), width, "…")},
			chrome{text: ansi.Truncate(t.dim().Render(clean(rev.Objective)), width, "…"), optional: true})
		if rev.Recovery != nil {
			text := t.fg(t.warn()).Render(clean("Recovery (" + rev.Recovery.Phase + "): " + rev.Recovery.Detail))
			rows = append(rows, chrome{text: ansi.Truncate(text, width, "…")})
		}
		if rev.Runtime.PreviewURL != "" {
			text := t.fg(t.success()).Render("Preview: ") + clean(rev.Runtime.PreviewURL)
			rows = append(rows, chrome{text: ansi.Truncate(text, width, "…"), optional: true})
		}
	}
	return append(rows, chrome{text: "", optional: true}, chrome{text: m.tabs(width)})
}

// footer holds the message bar, the composer or its hints, and navigation.
func (m Model) footer(width int, compact bool) []string {
	t := m.Theme
	info := t.fg(t.success()).Render(clean(m.Notice))
	if m.Error != "" {
		info = t.fg(t.danger()).Render(clean("Error: " + m.Error))
	}
	lines := []string{ansi.Truncate(info, width, "…")}
	if m.Mode != "" {
		input := clean(m.Input) + t.fg(t.accent()).Render("▌")
		label := m.Mode
		if m.Mode == "new" && len(m.Workflows) > 1 {
			label = "new · " + clean(m.NewWorkflow) + " workflow (tab to change)"
		}
		if compact || width < 24 {
			lines = append(lines, ansi.Truncate(t.bold().Render(label+" ")+input, width, ""))
		} else {
			lines = append(lines, t.box(label, []string{input}, width)...)
		}
	} else {
		prompt := t.dim().Render("To ") + t.fg(t.accent()).Render(m.Recipient) +
			t.dim().Render(" · i chat · n new · r rewind · a approve · o artifact · d diff · p/P plugins · x cancel")
		if compact {
			prompt = t.dim().Render("i chat · o artifact")
		}
		lines = append(lines, ansi.Truncate(prompt, width, ""))
	}
	navigation := "↑/↓ runs  ←/→ stages  tab views  pgup/pgdn scroll  s recipient  [/] history  ,/. artifacts  g open PR  T theme  q detach"
	if compact {
		navigation = "tab views · q detach"
	}
	return append(lines, ansi.Truncate(t.dim().Render(navigation), width, ""))
}

func (m Model) View() tea.View {
	width, height := max(10, m.Width), max(5, m.Height)
	compact := width < 70 || height < 18
	t := m.Theme
	foot := m.footer(width, compact)
	var head []string
	if compact {
		title := "envctl · " + panels[m.Panel]
		if r := m.current(); r != nil {
			title = clean(r.Name) + " · " + m.nodeID() + " · " + panels[m.Panel] + m.revisionLabel()
		}
		head = []string{ansi.Truncate(t.bold().Render(title), width, "…")}
	} else {
		for _, row := range m.headerRows() {
			head = append(head, row.text)
		}
	}
	detail := strings.Split(clean(m.details()), "\n")
	room := max(0, height-len(head)-len(foot))
	framed := !compact && room >= 3
	if framed {
		room -= 2
	}
	offset := min(m.Offset, max(0, len(detail)-room))
	body := make([]string, 0, room)
	title := panels[m.Panel]
	if m.Picker != nil {
		title = "Theme"
		body = m.pickerLines(width, room)
	}
	for i := len(body); i < room; i++ {
		line := ""
		if m.Picker == nil && offset+i < len(detail) {
			line = ansi.Truncate(t.severity(detail[offset+i]).Render(detail[offset+i]), width-2, "…")
		}
		body = append(body, line)
	}
	if framed {
		body = t.box(title, body, width)
	}
	lines := append(append(head, body...), foot...)
	if len(lines) > height {
		lines = lines[:height]
	}
	for i := range lines {
		lines[i] = ansi.Truncate(lines[i], width, "")
	}
	// A painted theme owns the whole screen, including rows below the frame.
	if t.colored() && t.pal.Background != "" {
		for len(lines) < height {
			lines = append(lines, "")
		}
		for i := range lines {
			lines[i] = t.canvas(lines[i], width)
		}
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
		hint := "Create a task with n, or use envctl run create --task ...\n\nRuns start from the envctl.yaml in " + m.Root + "."
		if _, err := os.Stat(filepath.Join(m.Root, "envctl.yaml")); err != nil {
			hint += "\nThere is no envctl.yaml there yet: add a version 2 workflow configuration first.\nSetup guide: https://sam-bretz.github.io/envctl/first-workflow/"
		}
		return hint
	}
	rev := m.viewRevision()
	node := m.nodeID()
	switch panels[m.Panel] {
	case "Conversation":
		agents := rev.Config.NodeAgents(node)
		lines := []string{fmt.Sprintf("Worker model: %s · Supervisor model: %s", formatModelTUI(agents.Worker.Model), formatModelTUI(agents.Supervisor.Model))}
		for _, a := range rev.Attempts {
			if a.Node == node {
				lines = append(lines, fmt.Sprintf("Worker attempt %d: %s", a.Number, a.State))
				if a.Error != "" {
					lines = append(lines, a.Error)
				}
				if a.Result != nil {
					lines = append(lines, a.Result.Summary, "Supervisor: "+a.Result.Review.Summary)
				}
				if p := a.Progress; a.State == "running" && p != nil {
					live := "Live: " + p.Phase
					if p.Generation > 0 {
						live += fmt.Sprintf(" (resume %d)", p.Generation)
					}
					if p.Detail != "" {
						live += " — " + p.Detail
					}
					live += " · updated " + p.UpdatedAt.Local().Format("15:04:05")
					lines = append(lines, live+"\n  "+strings.Join(p.Activity, "\n  "))
				}
			}
		}
		for _, msg := range rev.Messages {
			if msg.Node == "" || msg.Node == node {
				lines = append(lines, "To "+msg.Recipient+": "+msg.Body+"\n  ["+rev.MessageStatus(msg)+"]")
			}
		}
		if len(lines) == 1 {
			return lines[0] + "\n\nNo stage messages yet. Press i to address the " + m.Recipient + "."
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
			return "Branch runtime for " + node + ":\n" + runtimeSummary(child.Runtime)
		}
		return runtimeSummary(rev.Runtime)
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
func formatModelTUI(model string) string {
	if model == "" {
		return "harness default"
	}
	return model
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

// Options select the dashboard theme. Theme overrides the configuration file
// for this session; ConfigPath is where the picker saves a new choice.
type Options struct {
	Theme      string
	ConfigPath string
	Settings   Settings
	// Warning is shown on the first frame, for example an unreadable config.
	Warning string
}

func Run(ctx context.Context, api API, root string, opts Options) error {
	m := New(api, root)
	m.ConfigPath = opts.ConfigPath
	m.Theme.config = opts.Settings.Theme
	m.Theme.override = opts.Theme
	m.Theme.resolve()
	m.Error = opts.Warning
	_, err := tea.NewProgram(m, tea.WithContext(ctx)).Run()
	return err
}
