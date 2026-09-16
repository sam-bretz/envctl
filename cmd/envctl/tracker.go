package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/tracker"
	"github.com/sam-bretz/envctl/internal/tui"
	"github.com/sam-bretz/envctl/internal/workflow"
)

const trackerReference = "https://sam-bretz.github.io/envctl/config-reference/#tracker-version-2-experimental"

func trackerCmd(g *globals) *cobra.Command {
	c := &cobra.Command{Use: "tracker", Short: "Connect an issue tracker so linked issues follow a run"}
	c.AddCommand(&cobra.Command{
		Use:   "setup",
		Short: "Map workflow events to your tracker's statuses, choosing from the real ones",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A wizard reading answers from a pipe would block forever on a
			// question nobody can see, so refuse and say where to write it.
			if !term.IsTerminal(int(os.Stdin.Fd())) || !term.IsTerminal(int(os.Stdout.Fd())) {
				return fmt.Errorf("envctl tracker setup is interactive and needs a terminal; to configure a tracker without one, write tracker: in envctl.yaml by hand (see %s)", trackerReference)
			}
			return trackerSetup(cmd.Context(), setupIO{
				in: bufio.NewReader(cmd.InOrStdin()), out: cmd.OutOrStdout(), style: wizardStyle(),
				root: workflowRoot(cmd.Context(), g.dir), open: tracker.NewDirectory,
			})
		},
	})
	return c
}

// setupIO holds everything the wizard touches, so a test can drive the whole
// conversation with a scripted reader and a fake tracker.
type setupIO struct {
	in    *bufio.Reader
	out   io.Writer
	style style
	root  string
	open  func(workflow.TrackerConfig) (tracker.Directory, error)
}

type style struct{ title, accent, muted, ok lipgloss.Style }

// wizardStyle uses the dashboard's resolved theme, so the wizard looks like
// the rest of envctl rather than like a plain prompt.
func wizardStyle() style {
	settings := tui.Settings{}
	if path, err := tui.ConfigPath(); err == nil {
		if loaded, err := tui.LoadSettings(path); err == nil {
			settings = loaded
		}
	}
	p := settings.Theme.Resolve(lipgloss.HasDarkBackground(os.Stdin, os.Stdout))
	return style{
		title:  lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color(p.Accent)),
		accent: lipgloss.NewStyle().Foreground(lipgloss.Color(p.Accent)),
		muted:  lipgloss.NewStyle().Foreground(lipgloss.Color(p.Muted)),
		ok:     lipgloss.NewStyle().Foreground(lipgloss.Color(p.Success)),
	}
}

func trackerSetup(ctx context.Context, s setupIO) error {
	config, err := workflow.Load(s.root)
	if err != nil {
		return fmt.Errorf("%s: %w", s.root, err)
	}
	fmt.Fprintln(s.out, s.style.title.Render("Connect an issue tracker"))
	fmt.Fprintln(s.out, s.style.muted.Render("Nothing changes on any issue while this runs; every tracker call is a read."))

	provider, err := s.choose("Tracker", []string{"linear"}, 0)
	if err != nil {
		return err
	}
	current := ""
	if config.Tracker != nil {
		current = strings.TrimPrefix(config.Tracker.Credential, "file:")
	}
	path, err := s.ask("Path to a file holding the API key (the key itself is never asked for or shown)", current)
	if err != nil {
		return err
	}
	credential, err := absolute(path)
	if err != nil {
		return err
	}
	cfg := workflow.TrackerConfig{Provider: provider, Credential: "file:" + credential}
	dir, err := s.open(cfg)
	if err != nil {
		return err
	}
	if _, err = dir.Viewer(ctx); err != nil {
		return fmt.Errorf("the credential in %s did not work: %w", credential, err)
	}
	fmt.Fprintln(s.out, s.style.ok.Render("✓ Credential works"))

	teams, err := dir.Teams(ctx)
	if err != nil {
		return err
	}
	if len(teams) == 0 {
		return errors.New("this credential can see no teams")
	}
	names := make([]string, len(teams))
	for i, t := range teams {
		names[i] = fmt.Sprintf("%s (%s)", t.Name, t.Key)
	}
	picked, err := s.choose("Team", names, 0)
	if err != nil {
		return err
	}
	team := teams[indexOf(names, picked)]
	states, err := dir.States(ctx, team.ID)
	if err != nil {
		return err
	}

	mapping := map[string]workflow.TrackerStatusMapping{}
	for _, name := range config.WorkflowNames() {
		selected, err := config.SelectWorkflow(name)
		if err != nil {
			return err
		}
		stages := tracker.Stages(selected.Workflow)
		m, err := s.mapWorkflow(name, states, stages)
		if err != nil {
			return err
		}
		if len(tracker.Preview(m, stages)) > 0 || !empty(m) {
			mapping[name] = m
		}
	}

	block, err := tracker.TrackerBlock(provider, credential, mapping)
	if err != nil {
		return err
	}
	fmt.Fprintln(s.out, "\n"+s.style.title.Render("This will be written to envctl.yaml"))
	fmt.Fprintln(s.out, block)
	for _, name := range config.WorkflowNames() {
		selected, _ := config.SelectWorkflow(name)
		lines := tracker.Preview(mapping[name], tracker.Stages(selected.Workflow))
		fmt.Fprintln(s.out, "\n"+s.style.accent.Render("A typical "+name+" run would move the issue:"))
		if len(lines) == 0 {
			fmt.Fprintln(s.out, s.style.muted.Render("  never"))
		}
		for _, line := range lines {
			fmt.Fprintln(s.out, "  "+line)
		}
	}
	yes, err := s.ask("\nWrite it? (y/N)", "n")
	if err != nil {
		return err
	}
	if !strings.EqualFold(strings.TrimSpace(yes), "y") {
		fmt.Fprintln(s.out, s.style.muted.Render("Nothing written."))
		return nil
	}
	file := filepath.Join(s.root, manifest.FileName)
	raw, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	updated, err := tracker.SpliceTracker(raw, block)
	if err != nil {
		return err
	}
	info, err := os.Stat(file)
	if err != nil {
		return err
	}
	if err = os.WriteFile(file, updated, info.Mode().Perm()); err != nil {
		return err
	}
	// Prove the file still loads before saying it worked.
	if _, err = workflow.Load(s.root); err != nil {
		return fmt.Errorf("wrote %s but it no longer loads: %w", file, err)
	}
	fmt.Fprintln(s.out, s.style.ok.Render("✓ Written to "+file))
	return nil
}

// mapWorkflow asks for one workflow's mapping, with the suggested defaults
// preselected so pressing Enter throughout gives a working result.
func (s setupIO) mapWorkflow(name string, states []tracker.State, stages []tracker.Stage) (workflow.TrackerStatusMapping, error) {
	fmt.Fprintln(s.out, "\n"+s.style.title.Render("Workflow: "+name))
	m := tracker.DefaultMapping(states, stages)
	choices := append([]string{"(leave unmapped)"}, stateNames(states)...)
	ask := func(event string, current *string) error {
		picked, err := s.choose(event, choices, max(0, indexOf(choices, *current)))
		if err != nil {
			return err
		}
		if picked == choices[0] {
			*current = ""
		} else {
			*current = picked
		}
		return nil
	}
	for _, q := range []struct {
		event string
		field *string
	}{
		{"Run starts", &m.RunStarted},
		{"Approved", &m.Approved},
		{"Pull request published", &m.PRPublished},
		{"Needs attention", &m.NeedsAttention},
		{"Cancelled", &m.Cancelled},
		{"Rewound", &m.Rewound},
	} {
		if err := ask(q.event, q.field); err != nil {
			return m, err
		}
	}
	for _, stage := range stages {
		if !stage.Gated {
			continue
		}
		value := m.AwaitingApproval[stage.ID]
		if err := ask(stage.ID+" waits for approval", &value); err != nil {
			return m, err
		}
		m.AwaitingApproval = setOrDelete(m.AwaitingApproval, stage.ID, value)
	}
	detail, err := s.ask("Also map each stage starting and being accepted? (y/N)", "n")
	if err != nil {
		return m, err
	}
	if strings.EqualFold(strings.TrimSpace(detail), "y") {
		for _, stage := range stages {
			started, accepted := m.StageStarted[stage.ID], m.StageAccepted[stage.ID]
			if err = ask(stage.ID+" starts", &started); err != nil {
				return m, err
			}
			if err = ask(stage.ID+" is accepted", &accepted); err != nil {
				return m, err
			}
			m.StageStarted = setOrDelete(m.StageStarted, stage.ID, started)
			m.StageAccepted = setOrDelete(m.StageAccepted, stage.ID, accepted)
		}
	}
	return m, nil
}

// choose shows a numbered list and returns the chosen entry. Choosing from a
// list rather than typing means a status name can never be misspelled.
func (s setupIO) choose(question string, options []string, preselected int) (string, error) {
	fmt.Fprintln(s.out, s.style.accent.Render(question))
	for i, o := range options {
		marker := "  "
		if i == preselected {
			marker = s.style.ok.Render("› ")
		}
		fmt.Fprintf(s.out, "%s%d. %s\n", marker, i+1, o)
	}
	for {
		answer, err := s.ask("Choose", strconv.Itoa(preselected+1))
		if err != nil {
			return "", err
		}
		n, err := strconv.Atoi(strings.TrimSpace(answer))
		if err == nil && n >= 1 && n <= len(options) {
			return options[n-1], nil
		}
		fmt.Fprintf(s.out, "Enter a number from 1 to %d.\n", len(options))
	}
}

func (s setupIO) ask(question, fallback string) (string, error) {
	if fallback != "" {
		fmt.Fprintf(s.out, "%s %s: ", question, s.style.muted.Render("["+fallback+"]"))
	} else {
		fmt.Fprintf(s.out, "%s: ", question)
	}
	line, err := s.in.ReadString('\n')
	if err != nil && !(errors.Is(err, io.EOF) && line != "") {
		return "", fmt.Errorf("setup ended before it was answered: %w", err)
	}
	if answer := strings.TrimSpace(line); answer != "" {
		return answer, nil
	}
	return fallback, nil
}

func absolute(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("a credential file path is required")
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path = filepath.Join(home, rest)
	}
	return filepath.Abs(path)
}

func stateNames(states []tracker.State) []string {
	out := make([]string, len(states))
	for i, s := range states {
		out[i] = s.Name
	}
	return out
}

func indexOf(options []string, want string) int {
	for i, o := range options {
		if o == want {
			return i
		}
	}
	return -1
}

func setOrDelete(m map[string]string, key, value string) map[string]string {
	if value == "" {
		delete(m, key)
		if len(m) == 0 {
			return nil
		}
		return m
	}
	if m == nil {
		m = map[string]string{}
	}
	m[key] = value
	return m
}

func empty(m workflow.TrackerStatusMapping) bool {
	return m.RunStarted == "" && m.Approved == "" && m.PRPublished == "" && m.NeedsAttention == "" &&
		m.Cancelled == "" && m.Rewound == "" && len(m.StageStarted) == 0 && len(m.StageAccepted) == 0 && len(m.AwaitingApproval) == 0
}
