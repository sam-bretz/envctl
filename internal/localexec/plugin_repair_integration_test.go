package localexec

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/plugin"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealPluginRepairsDisappearedResource(t *testing.T) {
	if os.Getenv("ENVCTL_PLUGIN_TEST") != "1" {
		t.Skip("opt-in real plugin resource repair")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("explicit owned VM required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b, a := fixture(t)
	b.Provider = vm.NewLima(state)
	a.Revision.Runtime.ID = name
	// Global fixture identities are distinct so no previous guest journal can be reused.
	a.Run.ID, a.Revision.ID = workflow.ID("run"), workflow.ID("rev")
	source := t.TempDir()
	desc := plugin.Descriptor{ID: "repair", Version: "1.0.0", Protocol: 1, Provides: []string{"fixture.repair"}, Operations: []string{"prepare", "probe", "cleanup"}, Command: []string{"python3", "main.py"}}
	raw, _ := json.Marshal(desc)
	if err := os.WriteFile(filepath.Join(source, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.py"), []byte(resourceRepairPlugin), 0600); err != nil {
		t.Fatal(err)
	}
	a.Revision.Config.Plugins = []workflow.PluginRef{{ID: "repair", Source: source, Version: "1.0.0"}}
	c, lock, err := plugin.Freeze(filepath.Join(b.Store.Dir, "plugins"), a.Revision.Config)
	if err != nil {
		t.Fatal(err)
	}
	a.Revision.Config = c
	binding := lock.Bindings[0]
	root := "/work/envctl/plugins/" + a.Revision.ID
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := b.Provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", root}}); err != nil {
			t.Error(err)
		}
	})
	check := func(want bool) {
		t.Helper()
		probes, err := b.pluginProbes(ctx, a, []workflow.Probe{{Capability: "fixture.repair"}})
		if err != nil || probes[0].Passed != want {
			t.Fatalf("readiness=%v want %v: %v", probes, want, err)
		}
	}
	check(true)
	original, err := b.readPluginOperation(a, binding, "prepare")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "rm", "--", root + "/repair/state/resource"}}); err != nil {
		t.Fatal(err)
	}
	expirePluginProbe(t, b, a, binding)
	check(false)
	repair, _, err := b.readPluginRepair(a, binding)
	if err != nil || repair.Trigger == "" || repair.Previous != original.ID {
		t.Fatal("repair intent missing", err)
	}
	check(false) // backoff retains failed readiness
	b = New(b.Store)
	b.Provider = vm.NewLima(state)
	timer := time.NewTimer(time.Until(repair.RetryAt) + 20*time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	check(true)
	check(true)
	restored, err := b.readPluginOperation(a, binding, "probe")
	if err != nil || restored.ID == repair.Trigger || restored.Response.Output["effects"] != float64(2) {
		t.Fatal("fresh resource verification or effect count incorrect", err)
	}
	prepared, err := b.readPluginOperation(a, binding, "prepare")
	if err != nil || prepared.ID == original.ID || !prepared.Response.OK {
		t.Fatal("missing replacement preparation", err)
	}
	if _, err := b.Store.Artifact(original.Evidence.Digest); err != nil {
		t.Fatal("original preparation evidence lost", err)
	}
	if err := b.cleanupPlugins(ctx, a); err != nil {
		t.Fatal(err)
	}
	t.Log("deleted prepared resource detected by real guest probe; durable repair survived backend recreation; exactly one replacement effect and fresh probe restored readiness; cleanup passed")
}

const resourceRepairPlugin = `import json,os,pathlib,sys
r=json.load(sys.stdin);root=pathlib.Path(os.environ['ENVCTL_PLUGIN_STATE'])
resource=root/'resource';journal=root/'operations.json'
ops=json.loads(journal.read_text()) if journal.exists() else []
if r['operation']=='prepare':
 if r['operation_id'] not in ops:
  ops.append(r['operation_id'])
  tmp=journal.with_suffix('.tmp')
  with tmp.open('w') as f:json.dump(ops,f);f.flush();os.fsync(f.fileno())
  os.replace(tmp,journal)
 resource.write_text('ready')
if r['operation']=='cleanup':resource.unlink(missing_ok=True)
ok=resource.exists() if r['operation']=='probe' else True
response={'protocol':1,'ok':ok,'detail':'resource present' if ok else 'resource disappeared','output':{'effects':len(ops)}}
if not ok:response['recovery']='prepare'
print(json.dumps(response))
`
