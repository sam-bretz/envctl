package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealGuestPluginLifecycleAndReconnect(t *testing.T) {
	if os.Getenv("ENVCTL_PLUGIN_TEST") != "1" {
		t.Skip("opt-in real guest plugin lifecycle")
	}
	state, runtime := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || runtime == "" {
		t.Fatal("select the owned acceptance VM explicitly")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	dir := t.TempDir()
	desc := Descriptor{ID: "fixture", Version: "1.0.0", Protocol: 1, Provides: []string{"fixture.ready"}, Command: []string{"python3", "main.py"}, Operations: []string{"prepare", "probe", "execute", "renew", "cleanup"}}
	raw, _ := json.Marshal(desc)
	if err := os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.py"), []byte(realPlugin), 0600); err != nil {
		t.Fatal(err)
	}
	c, lock, err := Freeze(t.TempDir(), workflow.Config{Plugins: []workflow.PluginRef{{ID: "fixture", Version: "1.0.0", Source: dir}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = c
	b := lock.Bindings[0]
	g := Guest{Client: guestjob.Client{Provider: vm.NewLima(state), Runtime: runtime}, RunID: workflow.ID("run"), Revision: workflow.ID("rev")}
	guestDir, _ := g.directory(b)
	t.Cleanup(func() {
		cleanCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := g.Client.Provider.Exec(cleanCtx, runtime, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", guestDir}}); err != nil {
			t.Error("plugin fixture cleanup failed")
		}
	})
	secret := "synthetic-credential-\"-with-escaping"
	credentialAvailable := true
	resolutions := 0
	g.ResolveCredentials = func() (map[string]string, error) {
		resolutions++
		if !credentialAvailable {
			return nil, errors.New("synthetic credential is unavailable")
		}
		return map[string]string{"token": secret}, nil
	}
	call := func(op, id string) (Response, []byte, error) {
		return Call(ctx, g.Execute, b, Request{Protocol: 1, Operation: op, OperationID: id, RunID: g.RunID, Revision: g.Revision, Credentials: map[string]string{"token": secret}})
	}
	res, _, err := call("probe", "before")
	if err != nil || res.OK {
		t.Fatal("unprepared capability admitted", err)
	}
	res, _, err = call("prepare", "prepare")
	if err != nil || !res.OK {
		t.Fatal("prepare", err)
	}
	res, evidence, err := call("probe", "after")
	if err != nil || !res.OK || strings.Contains(string(evidence), secret) || res.Output["token"] != "[redacted]" {
		t.Fatal("probe/redaction", err)
	}
	// A real operation is cancelled only at its transport caller after its
	// systemd job is observed running. The guest operation continues.
	req := Request{Protocol: 1, Operation: "execute", OperationID: "execute", RunID: g.RunID, Revision: g.Revision, Credentials: map[string]string{"token": secret}}
	input, _ := json.Marshal(req)
	job := "plugin_" + workflow.Digest(struct{ Run, Revision, Plugin, Operation string }{g.RunID, g.Revision, b.Ref.ID, req.OperationID})[:40]
	short, disconnect := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, _, err := g.Execute(short, b, input); done <- err }()
	observed := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		status, err := g.Client.Poll(ctx, job, 0)
		if err != nil {
			disconnect()
			<-done
			t.Fatal(err)
		}
		if status.State == "running" {
			observed = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	disconnect()
	if err := <-done; err == nil || !observed {
		t.Fatal("did not disconnect during real operation", err)
	}
	// Reconnect must validate non-secret inputs even if credentials rotated,
	// then observe the original operation without reading replacement secrets.
	beforeResolutions := resolutions
	secret = "rotated-synthetic-credential-\"-with-escaping"
	changed := req
	changed.Input = map[string]any{"changed": true}
	changedRaw, _ := json.Marshal(changed)
	if _, _, err := g.Execute(ctx, b, changedRaw); err == nil {
		t.Fatal("changed operation input accepted on reconnect")
	}
	credentialAvailable = false
	res, _, err = call("execute", "execute")
	if err != nil || !res.OK || res.Output["executions"] != float64(1) || res.Output["credential_generation"] != float64(1) || resolutions != beforeResolutions || res.Output["token"] != "[redacted]" {
		t.Fatal("reconnect duplicated or lost operation", err, res.Output["executions"])
	}
	if _, _, err := call("renew", "renew"); err == nil {
		t.Fatal("new operation dispatched without credentials")
	}
	credentialAvailable = true
	if _, err = os.Stat(filepath.Join(dir, "state.json")); !os.IsNotExist(err) {
		t.Fatal("plugin wrote into host package")
	}
	res, _, err = call("renew", "renew")
	if err != nil || !res.OK || res.Output["credential_generation"] != float64(2) {
		t.Fatal("renew", err)
	}
	res, _, err = call("cleanup", "cleanup")
	if err != nil || !res.OK {
		t.Fatal("cleanup", err)
	}
	res, _, err = call("probe", "after-cleanup")
	if err != nil || res.OK {
		t.Fatal("cleanup left capability available", err)
	}
	// Neither a changed package nor a platform mismatch is silently accepted.
	if err = g.Client.Provider.Exec(ctx, runtime, vm.Command{Args: []string{"sudo", "chmod", "644", guestDir + "/package/main.py"}}); err != nil {
		t.Fatal(err)
	}
	if err = g.Install(ctx, b); err == nil {
		t.Fatal("guest package tampering went undetected")
	}
	t.Log("real guest lifecycle, disconnect and credential rotation/removal, frozen reconnect, fresh renewal, single execution, redaction and integrity passed")
}

const realPlugin = `import json,os,sys,time
req=json.load(sys.stdin);root=os.environ['ENVCTL_PLUGIN_STATE'];path=root+'/state.json'
state=json.load(open(path)) if os.path.exists(path) else {'ready':False,'executions':0}
op=req['operation']
if op=='prepare':state['ready']=True
if op=='execute':
 state['executions']+=1
 with open(path,'w') as f:json.dump(state,f)
 time.sleep(3)
if op=='cleanup':state['ready']=False
with open(path,'w') as f:json.dump(state,f)
token=req.get('credentials',{}).get('token','')
sys.stderr.write(token)
print(json.dumps({'protocol':1,'ok':state['ready'] if op=='probe' else True,'detail':'completed '+op,'expires_seconds':60,'output':{'executions':state['executions'],'token':token,'credential_generation':2 if token.startswith('rotated-') else 1}}))
`
