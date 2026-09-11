package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/plugin"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealPluginTerminalRecoveryPreservesOperationIdentity(t *testing.T) {
	if os.Getenv("ENVCTL_PLUGIN_TEST") != "1" {
		t.Skip("opt-in real plugin failure/recovery")
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
	source := t.TempDir()
	desc := plugin.Descriptor{ID: "recovery", Version: "1.0.0", Protocol: 1, Provides: []string{"fixture.recovery"}, Credentials: []string{"token"}, Operations: []string{"probe", "execute"}, Command: []string{"python3", "main.py"}}
	raw, _ := json.Marshal(desc)
	if err := os.WriteFile(filepath.Join(source, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "main.py"), []byte(recoveringPlugin), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ENVCTL_PLUGIN_RECOVERY_TOKEN", "synthetic-private-value")
	a.Revision.Config.Plugins = []workflow.PluginRef{{ID: "recovery", Source: source, Version: "1.0.0", Credentials: map[string]string{"token": "env:ENVCTL_PLUGIN_RECOVERY_TOKEN"}}}
	a.Revision.Config.Limits.MaxAttempts = 3
	c, lock, err := plugin.Freeze(filepath.Join(b.Store.Dir, "plugins"), a.Revision.Config)
	if err != nil {
		t.Fatal(err)
	}
	a.Revision.Config = c
	binding := lock.Bindings[0]
	t.Cleanup(func() {
		clean, stop := context.WithTimeout(context.Background(), 20*time.Second)
		defer stop()
		if err := b.Provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "python3", "-c", "import shutil,sys; shutil.rmtree(sys.argv[1],ignore_errors=True)", "/work/envctl/plugins/" + a.Revision.ID}}); err != nil {
			t.Error(err)
		}
	})
	var operationID string
	for generation := 0; generation < 2; generation++ {
		record, err := b.callPlugin(ctx, a, binding, "execute", true)
		var terminal *plugin.TerminalFailure
		if !errors.As(err, &terminal) || record.Generation != generation+1 || len(record.Failures) != generation+1 || record.Response != nil {
			t.Fatal("terminal failure was not retained", err)
		}
		if generation == 0 {
			operationID = record.ID
		}
		if record.ID != operationID {
			t.Fatal("recovery changed external operation identity")
		}
		for _, failure := range record.Failures {
			evidence, err := b.Store.Artifact(failure.Evidence.Digest)
			if err != nil || strings.Contains(string(evidence), "synthetic-private-value") {
				t.Fatal("failure evidence missing or exposed credential", err)
			}
		}
		held, err := b.callPlugin(ctx, a, binding, "execute", true)
		if err == nil || held.Generation != record.Generation {
			t.Fatal("recovery ignored backoff")
		}
		b = New(b.Store)
		b.Provider = vm.NewLima(state)
		wait := time.NewTimer(time.Until(record.RetryAt) + 20*time.Millisecond)
		select {
		case <-ctx.Done():
			wait.Stop()
			t.Fatal(ctx.Err())
		case <-wait.C:
		}
	}
	completed, err := b.callPlugin(ctx, a, binding, "execute", true)
	if err != nil || completed.Response == nil || !completed.Response.OK {
		t.Fatal("recovered process failed", err)
	}
	if completed.ID != operationID || completed.Response.Output["effects"] != float64(1) || completed.Response.Output["processes"] != float64(3) {
		t.Fatal("recovery duplicated external effect or lost process lineage")
	}
	replay, err := b.callPlugin(ctx, a, binding, "execute", true)
	if err != nil || replay.Evidence.Digest != completed.Evidence.Digest || replay.Generation != 2 {
		t.Fatal("success replay launched another process", err)
	}
	t.Log("process exit and invalid protocol recovered across backend recreation; stable operation identity performed one effect across three processes; failure evidence, backoff and replay verified")
}

const recoveringPlugin = `import json,os,pathlib,sys
r=json.load(sys.stdin)
p=pathlib.Path(os.environ['ENVCTL_PLUGIN_STATE'])/'effect.json'
if p.exists():
 s=json.loads(p.read_text())
 if s['operation']!=r['operation_id']:raise RuntimeError('operation identity changed')
else:s={'operation':r['operation_id'],'effects':1,'processes':0}
s['processes']+=1
tmp=p.with_suffix('.tmp')
with tmp.open('w') as f:json.dump(s,f);f.flush();os.fsync(f.fileno())
os.replace(tmp,p)
fd=os.open(p.parent,os.O_RDONLY);os.fsync(fd);os.close(fd)
if s['processes']==1:
 print(r['credentials']['token'],file=sys.stderr);sys.exit(7)
if s['processes']==2:
 print('invalid protocol response');sys.exit(0)
print(json.dumps({'protocol':1,'ok':True,'detail':'recovered','output':s}))
`
