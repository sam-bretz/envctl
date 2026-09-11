package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/plugin"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Simulates journaled guest processes, including a lost observation after a
// successful submission. Unique job identities count actual process launches.
type pluginJobs struct {
	vm.Provider
	jobs           map[string]string
	fail, losePoll bool
	respond        func(plugin.Request) plugin.Response
	responses      map[string]string
	loseReconnect  bool
}

func (p *pluginJobs) Exec(_ context.Context, _ string, c vm.Command) error {
	if len(c.Args) == 5 && c.Args[4] == guestjob.RunnerPath {
		if p.loseReconnect {
			p.loseReconnect = false
			return errors.New("reconnect transport lost")
		}
		var candidate guestjob.Request
		if err := json.NewDecoder(c.Stdin).Decode(&candidate); err != nil {
			return err
		}
		prior, ok := p.jobs[candidate.ID]
		if !ok {
			_, err := io.WriteString(c.Stdout, "missing\n")
			return err
		}
		var original, next map[string]any
		if json.Unmarshal([]byte(prior), &original) != nil || json.Unmarshal([]byte(candidate.Input), &next) != nil {
			return errors.New("invalid fixture reconnect request")
		}
		delete(original, "credentials")
		delete(next, "credentials")
		if workflow.Digest(original) != workflow.Digest(next) {
			return errors.New("job input changed")
		}
		_, err := io.WriteString(c.Stdout, "existing\n")
		return err
	}
	if len(c.Args) != 4 || c.Args[2] != guestjob.RunnerPath {
		return nil // package transfer; its integrity has separate guest coverage
	}
	var r struct {
		ID     string `json:"id"`
		Input  string `json:"input"`
		Cursor int64  `json:"cursor"`
	}
	if err := json.NewDecoder(c.Stdin).Decode(&r); err != nil {
		return err
	}
	s := guestjob.Status{ID: r.ID, State: "completed"}
	if c.Args[3] == "submit" {
		if prior, ok := p.jobs[r.ID]; ok && prior != r.Input {
			return errors.New("job request changed")
		}
		if _, exists := p.jobs[r.ID]; !exists && p.respond != nil {
			var request plugin.Request
			if err := json.Unmarshal([]byte(r.Input), &request); err != nil {
				return err
			}
			response, err := json.Marshal(p.respond(request))
			if err != nil {
				return err
			}
			if p.responses == nil {
				p.responses = map[string]string{}
			}
			p.responses[r.ID] = string(response)
		}
		p.jobs[r.ID] = r.Input
	} else {
		if p.losePoll {
			p.losePoll = false
			return errors.New("observation connection lost")
		}
		if p.fail {
			s.State = "failed"
		} else if r.Cursor == 0 {
			response := `{"protocol":1,"ok":true,"detail":"ready"}`
			if p.respond != nil {
				response = p.responses[r.ID]
			}
			envelope, _ := json.Marshal(map[string]any{"stdout": response, "stderr": "", "exit": 0})
			s.Output = string(envelope)
			s.Cursor = int64(len(envelope))
		}
	}
	return json.NewEncoder(c.Stdout).Encode(s)
}

func pluginFixture(t *testing.T, operations ...string) (*Backend, engine.Assignment, plugin.Binding, *pluginJobs) {
	t.Helper()
	b, a := fixture(t)
	root := t.TempDir()
	descriptor := plugin.Descriptor{ID: "fixture", Version: "1.0.0", Protocol: 1, Provides: []string{"fixture.ready"}, Command: []string{"python3", "main.py"}, Operations: []string{"probe"}}
	descriptor.Operations = append(descriptor.Operations, operations...)
	raw, _ := json.Marshal(descriptor)
	if err := os.WriteFile(filepath.Join(root, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	c, lock, err := plugin.Freeze(filepath.Join(b.Store.Dir, "plugins"), workflow.Config{Plugins: []workflow.PluginRef{{ID: "fixture", Source: root, Version: "1.0.0"}}})
	if err != nil {
		t.Fatal(err)
	}
	a.Revision.Config.Plugins = c.Plugins
	p := &pluginJobs{jobs: map[string]string{}}
	b.Provider = p
	return b, a, lock.Bindings[0], p
}

func TestPluginTransportRecoveryReusesPendingProcess(t *testing.T) {
	b, a, binding, p := pluginFixture(t)
	p.losePoll = true
	first, err := b.callPlugin(context.Background(), a, binding, "probe", true)
	var terminal *plugin.TerminalFailure
	if err == nil || errors.As(err, &terminal) || first.Generation != 0 || len(first.Failures) != 0 || len(p.jobs) != 1 {
		t.Fatal("unknown outcome incorrectly allowed a new process", err)
	}
	b = New(b.Store)
	b.Provider = p
	recovered, err := b.callPlugin(context.Background(), a, binding, "probe", true)
	if err != nil || recovered.Response == nil || !recovered.Response.OK || recovered.ID != first.ID || len(p.jobs) != 1 {
		t.Fatal("transport reconnect duplicated process or lost output", err)
	}
}

func TestPluginRecoveryBudgetAndFailureHistory(t *testing.T) {
	b, a, binding, p := pluginFixture(t)
	a.Revision.Config.Limits.MaxAttempts = 2
	p.fail = true
	var operation string
	filename, err := b.pluginPath(a, binding, "probe")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		record, err := b.callPlugin(context.Background(), a, binding, "probe", true)
		var terminal *plugin.TerminalFailure
		if !errors.As(err, &terminal) || record.Generation != i+1 || len(record.Failures) != i+1 {
			t.Fatal("failure not journaled", err)
		}
		if i == 0 {
			operation = record.ID
		}
		if record.ID != operation {
			t.Fatal("external operation identity changed")
		}
		record.RetryAt = time.Now().Add(-time.Second)
		if err := atomicJSON(filename, record); err != nil {
			t.Fatal(err)
		}
		b = New(b.Store)
		b.Provider = p
	}
	p.fail = false
	record, err := b.callPlugin(context.Background(), a, binding, "probe", true)
	if err == nil || !strings.Contains(err.Error(), "exhausted") || len(p.jobs) != 2 || record.Response != nil {
		t.Fatal("exhausted operation dispatched", err)
	}
	for _, failure := range record.Failures {
		if _, err := b.Store.Artifact(failure.Evidence.Digest); err != nil {
			t.Fatal("lost failure evidence", err)
		}
	}
}

func TestPluginFreshProbeArchivesCompletedOperation(t *testing.T) {
	b, a, binding, p := pluginFixture(t)
	first, err := b.callPlugin(context.Background(), a, binding, "probe", true)
	if err != nil {
		t.Fatal(err)
	}
	second, err := b.callPlugin(context.Background(), a, binding, "probe", false)
	if err != nil || first.ID == second.ID || len(p.jobs) != 2 {
		t.Fatal("fresh probe did not get separate operation", err)
	}
	filename, _ := b.pluginPath(a, binding, "probe")
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filename), "history", first.ID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var previous pluginOperation
	if json.Unmarshal(raw, &previous) != nil || previous.ID != first.ID || previous.Evidence.Digest != first.Evidence.Digest {
		t.Fatal("prior operation evidence not retained")
	}
}

func TestPluginPendingLifecycleSurvivesCredentialRemoval(t *testing.T) {
	for _, operation := range []string{"prepare", "probe", "execute", "renew", "cleanup"} {
		t.Run(operation, func(t *testing.T) {
			b, a, binding, p := pluginFixture(t, "prepare", "execute", "renew", "cleanup")
			credential := filepath.Join(t.TempDir(), "credential")
			binding.Ref.Credentials = map[string]string{"token": "file:" + credential}
			if err := os.WriteFile(credential, []byte("initial-synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
			var observed []string
			p.respond = func(req plugin.Request) plugin.Response {
				observed = append(observed, req.Credentials["token"])
				return plugin.Response{Protocol: 1, OK: true, Detail: "verified"}
			}
			p.losePoll = true
			first, err := b.callPlugin(context.Background(), a, binding, operation, true)
			if err == nil || len(observed) != 1 {
				t.Fatal("fixture did not lose observation after submission")
			}
			if err := os.Remove(credential); err != nil {
				t.Fatal(err)
			}
			b = New(b.Store)
			b.Provider = p
			p.loseReconnect = true
			uncertain, err := b.callPlugin(context.Background(), a, binding, operation, true)
			if err == nil || uncertain.Generation != 0 || len(observed) != 1 {
				t.Fatal("unknown reconnect outcome restarted operation")
			}
			reconnected, err := b.callPlugin(context.Background(), a, binding, operation, true)
			if err != nil || reconnected.Response == nil || !reconnected.Response.OK || reconnected.ID != first.ID || len(observed) != 1 {
				t.Fatal("credential removal prevented frozen reconnect", err)
			}
			if _, err := b.callPlugin(context.Background(), a, binding, operation, false); err == nil || len(observed) != 1 {
				t.Fatal("new operation admitted without credentials")
			}
			if err := os.WriteFile(credential, []byte("rotated-synthetic"), 0600); err != nil {
				t.Fatal(err)
			}
			next, err := b.callPlugin(context.Background(), a, binding, operation, false)
			if err != nil || next.ID == first.ID || len(observed) != 2 || observed[0] != "initial-synthetic" || observed[1] != "rotated-synthetic" {
				t.Fatal("new operation did not use rotated credential", err)
			}
			filename, _ := b.pluginPath(a, binding, operation)
			for _, path := range []string{filename, filepath.Join(filepath.Dir(filename), "history", first.ID+".json")} {
				raw, err := os.ReadFile(path)
				if err != nil || strings.Contains(string(raw), "initial-synthetic") || strings.Contains(string(raw), "rotated-synthetic") {
					t.Fatal("host operation receipt missing or contains credentials")
				}
			}
		})
	}
}
