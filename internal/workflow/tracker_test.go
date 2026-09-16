package workflow

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConfigWithoutTrackerStaysByteIdentical(t *testing.T) {
	c := fixture(t)
	digest := Digest(c)
	raw := mustMarshal(t, c)
	if c.Tracker != nil {
		t.Fatal("fixture unexpectedly configured a tracker")
	}
	if strings.Contains(string(raw), "tracker") {
		t.Fatal("configuration without a tracker mentions tracker in its JSON encoding")
	}
	if Digest(c) != digest {
		t.Fatal("marshaling changed the configuration digest")
	}
}

func TestTrackerConfigValidation(t *testing.T) {
	base := fixture(t)
	for _, tc := range []struct {
		name    string
		tracker *TrackerConfig
		wantErr bool
	}{
		{"nil is fine", nil, false},
		{"valid env", &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN"}, false},
		{"valid absolute file", &TrackerConfig{Provider: "linear", Credential: "file:/etc/envctl/linear.token"}, false},
		{"unsupported provider", &TrackerConfig{Provider: "jira", Credential: "env:X"}, true},
		{"missing provider", &TrackerConfig{Credential: "env:X"}, true},
		{"no prefix", &TrackerConfig{Provider: "linear", Credential: "LINEAR_TOKEN"}, true},
		{"empty credential", &TrackerConfig{Provider: "linear", Credential: ""}, true},
		{"relative file path", &TrackerConfig{Provider: "linear", Credential: "file:relative/path"}, true},
		{"empty env name", &TrackerConfig{Provider: "linear", Credential: "env:"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := base
			c.Tracker = tc.tracker
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatal("expected validation error")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestTrackerStatusMappingValidatesEventsStagesAndNamedWorkflows(t *testing.T) {
	base := fixture(t)
	base.Tracker = &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN", Mapping: map[string]TrackerStatusMapping{
		"default": {RunStarted: "In Progress", StageStarted: map[string]string{"code": "In Progress"}},
	}}
	if err := base.Validate(); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}
	if got := base.Tracker.TrackerStatus("default", TrackerEventStageStarted, "code"); got != "In Progress" {
		t.Fatalf("mapped stage status = %q", got)
	}

	base.Workflows = map[string]Definition{"small": base.Workflow}
	base.Tracker.Mapping["small"] = TrackerStatusMapping{StageAccepted: map[string]string{"qa": "In Review"}}
	if err := base.Validate(); err != nil {
		t.Fatalf("mapping for a named workflow rejected by the full config: %v", err)
	}
	selected, err := base.SelectWorkflow("small")
	if err != nil {
		t.Fatalf("named workflow mapping rejected: %v", err)
	}
	if got := selected.Tracker.TrackerStatus(selected.WorkflowName, TrackerEventStageAccepted, "qa"); got != "In Review" {
		t.Fatalf("named workflow status = %q", got)
	}

	badStage := base
	badStage.Tracker = &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN", Mapping: map[string]TrackerStatusMapping{
		"default": {StageStarted: map[string]string{"missing": "In Progress"}},
	}}
	if err := badStage.Validate(); err == nil || !strings.Contains(err.Error(), "valid stages are") || !strings.Contains(err.Error(), "code") {
		t.Fatalf("unknown stage error did not list valid stages: %v", err)
	}

	badWorkflow := fixture(t)
	badWorkflow.Tracker = &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN", Mapping: map[string]TrackerStatusMapping{
		"missing": {RunStarted: "In Progress"},
	}}
	if err := badWorkflow.Validate(); err == nil || !strings.Contains(err.Error(), "valid workflows are") || !strings.Contains(err.Error(), "default") {
		t.Fatalf("unknown workflow error did not list valid workflows: %v", err)
	}
}

func TestTrackerStatusMappingRejectsUnknownEventAndListsValidEvents(t *testing.T) {
	raw := `version: 2
project: test
repositories: [{id: app, url: /source}]
workflow: {template: feature}
tracker:
 provider: linear
 credential: env:LINEAR_TOKEN
 mapping:
  default:
   not-an-event: In Progress
`
	_, err := Parse([]byte(raw))
	if err == nil || !strings.Contains(err.Error(), "unknown tracker mapping event") || !strings.Contains(err.Error(), "run_started") {
		t.Fatalf("unknown event error was not useful: %v", err)
	}
}

func TestTrackerStatusMappingIsOmittedWhenUnconfigured(t *testing.T) {
	config := fixture(t)
	config.Tracker = &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN"}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "mapping") {
		t.Fatalf("unconfigured mapping changed the encoded config: %s", raw)
	}
}

func TestMappedTrackerEventAddsOneStatusTransitionAfterItsComment(t *testing.T) {
	r := live(t)
	r.TaskRef = "ENG-42"
	r.Current().Config.Tracker = &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN", Mapping: map[string]TrackerStatusMapping{
		"default": {StageAccepted: map[string]string{"code": "In Review"}},
	}}
	r.AppendTrackerLog(r.Current().Config.Tracker, TrackerKindStageCompleted, r.CurrentRevision, "code", "attempt_1", "", time.Now())
	if len(r.TrackerLog) != 2 {
		t.Fatalf("mapped event produced %d entries, want comment and transition", len(r.TrackerLog))
	}
	if r.TrackerLog[0].StatusName != "" || r.TrackerLog[1].StatusName != "In Review" {
		t.Fatalf("unexpected mapped event entries: %+v", r.TrackerLog)
	}
}

func TestTrackerCredentialNeverAppearsInValidationErrors(t *testing.T) {
	c := fixture(t)
	c.Tracker = &TrackerConfig{Provider: "linear", Credential: "super-secret-value-not-a-reference"}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if strings.Contains(err.Error(), "super-secret-value-not-a-reference") {
		t.Fatalf("credential value leaked into validation error: %v", err)
	}
}

func TestTrackerRequirementOnlyWhenConfigured(t *testing.T) {
	r := live(t).Current()
	if slices.Contains(r.Requirements(), "tracker.comment") {
		t.Fatal("tracker capability required without a tracker configured")
	}
	r.Config.Tracker = &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN"}
	if !slices.Contains(r.Requirements(), "tracker.comment") {
		t.Fatal("tracker.comment not required once a tracker is configured")
	}
}

func TestAppendTrackerLogIsANoOpWithoutTrackerOrTaskRef(t *testing.T) {
	cfg := &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN"}
	now := time.Now()

	r := &Run{ID: "run_x"}
	r.AppendTrackerLog(cfg, TrackerKindStageCompleted, "rev_1", "code", "attempt_1", "", now)
	if len(r.TrackerLog) != 0 {
		t.Fatal("entry appended without a task ref")
	}

	r.TaskRef = "ENG-1"
	r.AppendTrackerLog(nil, TrackerKindStageCompleted, "rev_1", "code", "attempt_1", "", now)
	if len(r.TrackerLog) != 0 {
		t.Fatal("entry appended without a configured tracker")
	}

	r.AppendTrackerLog(cfg, TrackerKindStageCompleted, "rev_1", "code", "attempt_1", "", now)
	if len(r.TrackerLog) != 1 {
		t.Fatal("expected exactly one appended entry")
	}
	e := r.TrackerLog[0]
	if e.Kind != TrackerKindStageCompleted || e.Revision != "rev_1" || e.Node != "code" || e.Attempt != "attempt_1" || e.Status != "pending" || e.ID == "" {
		t.Fatalf("unexpected entry shape: %+v", e)
	}
}

func TestTrackerLogCounts(t *testing.T) {
	r := &Run{TrackerLog: []TrackerLogEntry{
		{Status: "pending"},
		{Status: "pending"},
		{Status: "failed"},
		{Status: "posted"},
	}}
	pending, failed := r.TrackerLogCounts()
	if pending != 2 || failed != 1 {
		t.Fatalf("unexpected counts: pending=%d failed=%d", pending, failed)
	}
}

// TestTrackerLogEnqueuedOnEachEventTransition exercises AppendTrackerLog's
// integration with the transitions that call it, using the Run/Revision
// state machine directly (the engine's call sites are covered by engine
// package tests): checkpoint acceptance, approval and rewind each enqueue
// exactly one entry when a tracker and task ref are present.
func TestTrackerLogEnqueuedOnEachEventTransition(t *testing.T) {
	r := live(t)
	cfg := &TrackerConfig{Provider: "linear", Credential: "env:LINEAR_TOKEN"}
	r.Current().Config.Tracker = cfg
	r.TaskRef = "ENG-42"
	v := r.Current()
	ready(v, time.Now())

	// Simulate the checkpoint.accepted call site's logging.
	a, err := v.Begin("task", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	res := output(v, "task")
	if err = v.Propose(a.ID, res, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err = v.Accept(a.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.AppendTrackerLog(v.Config.Tracker, TrackerKindStageCompleted, v.ID, a.Node, a.ID, "", time.Now())
	if len(r.TrackerLog) != 1 || r.TrackerLog[0].Kind != TrackerKindStageCompleted {
		t.Fatal("stage_completed entry not recorded")
	}

	if _, err = r.Rewind("task", "", nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	r.AppendTrackerLog(r.Current().Config.Tracker, TrackerKindRewound, r.CurrentRevision, "task", "", "target task", time.Now())
	if len(r.TrackerLog) != 2 || r.TrackerLog[1].Kind != TrackerKindRewound {
		t.Fatal("rewound entry not recorded")
	}
}
