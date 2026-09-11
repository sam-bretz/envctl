package localexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealInvocationPluginReadiness(t *testing.T) {
	if os.Getenv("ENVCTL_PLUGIN_TEST") != "1" {
		t.Skip("opt-in real guest plugin readiness")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("select the owned acceptance VM explicitly")
	}
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	dir := t.TempDir()
	desc := plugin.Descriptor{ID: "dataset", Version: "1.0.0", Protocol: 1, Provides: []string{"dataset.ready"}, Requires: []string{"runtime.compose"}, Credentials: []string{"token"}, Operations: []string{"prepare", "probe", "renew", "cleanup"}, Command: []string{"python3", "main.py"}}
	raw, _ := json.Marshal(desc)
	if err = os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "main.py"), []byte(readinessPlugin), 0600); err != nil {
		t.Fatal(err)
	}
	credential := filepath.Join(t.TempDir(), "credential")
	c := workflow.Config{Dir: dir, Plugins: []workflow.PluginRef{{ID: "dataset", Version: "1.0.0", Source: dir, Credentials: map[string]string{"token": "file:" + credential}}}}
	c, lock, err := plugin.Freeze(filepath.Join(store.Dir, "plugins"), c)
	if err != nil {
		t.Fatal(err)
	}
	a := engine.Assignment{Run: workflow.Run{ID: workflow.ID("run")}, Revision: workflow.Revision{ID: workflow.ID("rev"), Config: c, Runtime: workflow.RuntimeState{ID: name}}}
	b := New(store)
	b.Provider = vm.NewLima(state)
	probes := func(dependency bool) []workflow.Probe {
		return []workflow.Probe{{Capability: "runtime.compose", Passed: dependency, ExpiresAt: time.Now().Add(time.Minute)}, {Capability: "dataset.ready"}}
	}
	check := func(dependency, want bool) {
		t.Helper()
		got, err := b.pluginProbes(ctx, a, probes(dependency))
		if err != nil {
			t.Fatal(err)
		}
		if got[1].Passed != want {
			t.Fatal("plugin readiness mismatch:", got[1].Detail)
		}
	}
	check(false, false)
	filename, _ := b.pluginPath(a, lock.Bindings[0], "prepare")
	if _, err = os.Stat(filename); !os.IsNotExist(err) {
		t.Fatal("plugin dispatched before prerequisite probe passed")
	}
	check(true, false)
	if _, err = os.Stat(filename); !os.IsNotExist(err) {
		t.Fatal("plugin dispatched with missing credentials")
	}
	if err = os.WriteFile(credential, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	check(true, true)
	first, err := b.readPluginOperation(a, lock.Bindings[0], "prepare")
	if err != nil {
		t.Fatal(err)
	}
	// Recreate backend and expire the real lease; renewal has to execute in the
	// guest before the capability is admitted again.
	b = New(store)
	b.Provider = vm.NewLima(state)
	time.Sleep(1200 * time.Millisecond)
	check(true, true)
	renew, err := b.readPluginOperation(a, lock.Bindings[0], "renew")
	if err != nil || renew.Response == nil || !renew.Response.OK {
		t.Fatal("expired lease was not renewed", err)
	}
	again, err := b.readPluginOperation(a, lock.Bindings[0], "prepare")
	if err != nil || again.ID != first.ID {
		t.Fatal("restart repeated successful preparation", err)
	}
	if err = b.cleanupPlugins(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = b.cleanupPlugins(ctx, a); err != nil {
		t.Fatal("cleanup replay", err)
	}
	if err = b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", "/work/envctl/plugins/" + a.Revision.ID}}); err != nil {
		t.Fatal("plugin fixture cleanup", err)
	}
	t.Log("dependency and credential admission, real prepare/probe, lease renewal, backend recreation and idempotent cleanup passed")
}

const readinessPlugin = `import json,os,sys
req=json.load(sys.stdin);path=os.environ['ENVCTL_PLUGIN_STATE']+'/ready'
op=req['operation']
if op in ['prepare','renew']:open(path,'w').write('ready')
if op=='cleanup' and os.path.exists(path):os.unlink(path)
ok=os.path.exists(path) if op=='probe' else True
print(json.dumps({'protocol':1,'ok':ok,'detail':'completed '+op,'expires_seconds':1}))
`
