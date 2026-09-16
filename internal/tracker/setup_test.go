package tracker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// linearStates is a real Linear team's default board.
var linearStates = []State{
	{Name: "Backlog", Type: "backlog"},
	{Name: "Todo", Type: "unstarted"},
	{Name: "In Progress", Type: "started"},
	{Name: "In Review", Type: "started"},
	{Name: "Done", Type: "completed"},
	{Name: "Canceled", Type: "canceled"},
}

var featureStages = []Stage{
	{ID: "task"}, {ID: "plan"}, {ID: "design"}, {ID: "code"}, {ID: "qa"},
	{ID: "approved-change", Gated: true},
}

func TestAcceptingEveryDefaultGivesAWorkingMapping(t *testing.T) {
	m := DefaultMapping(linearStates, featureStages)
	// In Progress and In Review share the "started" type, so the name has to
	// separate them or the run would start straight into review.
	if m.RunStarted != "In Progress" {
		t.Fatalf("run started: %q", m.RunStarted)
	}
	if m.AwaitingApproval["approved-change"] != "In Review" {
		t.Fatalf("the gated stage should wait in review: %v", m.AwaitingApproval)
	}
	if m.PRPublished != "Done" || m.Cancelled != "Canceled" {
		t.Fatalf("terminal states: published %q cancelled %q", m.PRPublished, m.Cancelled)
	}
	// A rewind after Done must move the issue back, not leave it Done.
	if m.Rewound != "In Progress" {
		t.Fatalf("rewound would leave a reopened issue Done: %q", m.Rewound)
	}
	// Ungated stages have no review moment to map.
	if _, ok := m.AwaitingApproval["code"]; ok {
		t.Fatalf("mapped approval for a stage that never waits: %v", m.AwaitingApproval)
	}
}

func TestADefaultIsLeftUnmappedWhenTheTeamHasNoStateOfThatType(t *testing.T) {
	// A team with no review column must not have review guessed onto some
	// other started state.
	m := DefaultMapping([]State{{Name: "Doing", Type: "started"}, {Name: "Shipped", Type: "completed"}}, featureStages)
	if m.RunStarted != "Doing" || m.PRPublished != "Shipped" {
		t.Fatalf("available types not used: %+v", m)
	}
	if len(m.AwaitingApproval) != 0 {
		t.Fatalf("invented a review state: %v", m.AwaitingApproval)
	}
	if m.Cancelled != "" {
		t.Fatalf("invented a cancelled state: %q", m.Cancelled)
	}
}

func TestThePreviewListsTransitionsInTheOrderARunMakesThem(t *testing.T) {
	got := Preview(DefaultMapping(linearStates, featureStages), featureStages)
	want := []string{"run starts → In Progress", "approved-change waits for approval → In Review", "pull request published → Done"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("preview:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

const project = `# The project this repository builds.
version: 2
project: shop
repositories: [{id: app, url: /source}]
# Agents run on the default harness.
workflow: {template: feature}
`

func TestTheWrittenMappingLoadsValidatesAndKeepsTheRestOfTheFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "envctl.yaml")
	if err := os.WriteFile(path, []byte(project), 0o600); err != nil {
		t.Fatal(err)
	}
	block, err := TrackerBlock("linear", "/home/me/.config/linear.token", map[string]workflow.TrackerStatusMapping{
		"default": DefaultMapping(linearStates, featureStages),
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := SpliceTracker([]byte(project), block)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(updated), project) {
		t.Fatalf("the existing configuration was rewritten:\n%s", updated)
	}
	if err = os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := workflow.Load(dir)
	if err != nil {
		t.Fatalf("written mapping does not load: %v\n%s", err, updated)
	}
	if err = config.Validate(); err != nil {
		t.Fatalf("written mapping does not validate: %v", err)
	}
	if config.Tracker == nil || config.Tracker.Mapping["default"].PRPublished != "Done" {
		t.Fatalf("mapping lost on reload: %+v", config.Tracker)
	}
}

func TestRerunningTheWizardEditsTheMappingInsteadOfAddingASecond(t *testing.T) {
	first, _ := TrackerBlock("linear", "/tokens/a", map[string]workflow.TrackerStatusMapping{"default": {PRPublished: "Done"}})
	once, err := SpliceTracker([]byte(project), first)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := TrackerBlock("linear", "/tokens/b", map[string]workflow.TrackerStatusMapping{"default": {PRPublished: "Shipped"}})
	twice, err := SpliceTracker(once, second)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(twice), "tracker:"); n != 1 {
		t.Fatalf("tracker: written %d times:\n%s", n, twice)
	}
	if strings.Contains(string(twice), "Done") || !strings.Contains(string(twice), "Shipped") {
		t.Fatalf("the rerun did not replace the old mapping:\n%s", twice)
	}
	if !strings.Contains(string(twice), "# Agents run on the default harness.") {
		t.Fatalf("a comment outside the tracker block was lost:\n%s", twice)
	}
}

func TestTheCredentialValueNeverReachesTheConfiguration(t *testing.T) {
	// The wizard reads the token to check it, but only the reference is
	// written. A relative path is refused because the token must be locatable
	// from any checkout.
	block, err := TrackerBlock("linear", "/home/me/.config/linear.token", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(block, "credential: file:/home/me/.config/linear.token") {
		t.Fatalf("credential reference missing:\n%s", block)
	}
	if _, err = TrackerBlock("linear", "linear.token", nil); err == nil {
		t.Fatal("accepted a relative credential path")
	}
}

func TestAOneLineTrackerKeyIsRefusedRatherThanReformatted(t *testing.T) {
	raw := project + "tracker: {provider: linear, credential: 'file:/x'}\n"
	block, _ := TrackerBlock("linear", "/x", nil)
	if _, err := SpliceTracker([]byte(raw), block); err == nil || !strings.Contains(err.Error(), "one line") {
		t.Fatalf("flow-style tracker: not refused clearly: %v", err)
	}
}
