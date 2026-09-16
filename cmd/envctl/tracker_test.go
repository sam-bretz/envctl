package main

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/tracker"
	"github.com/sam-bretz/envctl/internal/workflow"
)

type fakeDirectory struct{ viewed bool }

func (f *fakeDirectory) Viewer(context.Context) (string, error) { f.viewed = true; return "me", nil }
func (f *fakeDirectory) Teams(context.Context) ([]tracker.Team, error) {
	return []tracker.Team{{ID: "t1", Key: "ENG", Name: "Engineering"}}, nil
}
func (f *fakeDirectory) States(context.Context, string) ([]tracker.State, error) {
	return []tracker.State{
		{Name: "Todo", Type: "unstarted"}, {Name: "In Progress", Type: "started"},
		{Name: "In Review", Type: "started"}, {Name: "Done", Type: "completed"}, {Name: "Canceled", Type: "canceled"},
	}, nil
}

const trackerProject = "# keep this comment\nversion: 2\nproject: shop\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\n"

const secret = "lin_api_THIS_MUST_NEVER_APPEAR"

func runSetup(t *testing.T, answers string) (string, string, *fakeDirectory) {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "envctl.yaml"), []byte(trackerProject), 0o600); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "linear.token")
	if err := os.WriteFile(token, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	fake := &fakeDirectory{}
	var out bytes.Buffer
	var opened workflow.TrackerConfig
	err := trackerSetup(context.Background(), setupIO{
		in:   bufio.NewReader(strings.NewReader(strings.ReplaceAll(answers, "TOKEN", token))),
		out:  &out,
		root: root,
		open: func(cfg workflow.TrackerConfig) (tracker.Directory, error) { opened = cfg; return fake, nil },
	})
	if err != nil {
		t.Fatalf("setup: %v\n%s", err, out.String())
	}
	if opened.Credential != "file:"+token {
		t.Fatalf("tracker opened with %q, want the file reference", opened.Credential)
	}
	written, _ := os.ReadFile(filepath.Join(root, "envctl.yaml"))
	return out.String(), string(written), fake
}

func TestAcceptingEveryDefaultWritesAWorkingMapping(t *testing.T) {
	// Tracker, credential path, team, then Enter through every question,
	// then "y" to write.
	answers := "\nTOKEN\n\n" + strings.Repeat("\n", 7) + "\n" + "y\n"
	out, written, fake := runSetup(t, answers)
	if !fake.viewed {
		t.Fatal("the credential was never checked")
	}
	if !strings.HasPrefix(written, "# keep this comment\n") {
		t.Fatalf("the file was rewritten rather than added to:\n%s", written)
	}
	for _, want := range []string{"run_started: In Progress", "pr_published: Done", "approved-change: In Review", "rewound: In Progress"} {
		if !strings.Contains(written, want) {
			t.Fatalf("missing %q in:\n%s", want, written)
		}
	}
	if !strings.Contains(out, "run starts → In Progress") {
		t.Fatalf("no transition preview shown:\n%s", out)
	}
}

func TestTheAPIKeyIsNeverWrittenOrShown(t *testing.T) {
	answers := "\nTOKEN\n\n" + strings.Repeat("\n", 7) + "\n" + "y\n"
	out, written, _ := runSetup(t, answers)
	if strings.Contains(out, secret) {
		t.Fatal("the API key was printed")
	}
	if strings.Contains(written, secret) {
		t.Fatal("the API key was written to envctl.yaml")
	}
}

func TestDecliningWritesNothing(t *testing.T) {
	answers := "\nTOKEN\n\n" + strings.Repeat("\n", 7) + "\n" + "n\n"
	out, written, _ := runSetup(t, answers)
	if written != trackerProject {
		t.Fatalf("declining changed envctl.yaml:\n%s", written)
	}
	if !strings.Contains(out, "Nothing written") {
		t.Fatalf("declining was not acknowledged:\n%s", out)
	}
}

func TestSetupRefusesWithoutATerminalInsteadOfHanging(t *testing.T) {
	// go test's stdin is not a terminal, which is exactly the scripted or CI
	// case the command must refuse.
	g := &globals{dir: t.TempDir()}
	cmd := trackerCmd(g)
	cmd.SetArgs([]string{"setup"})
	cmd.SetIn(strings.NewReader(""))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "needs a terminal") || !strings.Contains(err.Error(), "config-reference") {
		t.Fatalf("expected a refusal pointing to the config reference, got %v", err)
	}
}
