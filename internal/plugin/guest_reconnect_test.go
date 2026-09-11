package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/guestjob"
	vm "github.com/sam-bretz/envctl/internal/runtime"
)

type reconnectExecutor struct{ python, runner string }

func (e reconnectExecutor) Exec(ctx context.Context, _ string, c vm.Command) error {
	cmd := exec.CommandContext(ctx, e.python, "-B", "-c", c.Args[3], e.runner)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = c.Stdin, c.Stdout, c.Stderr
	return cmd.Run()
}

// Exercise the actual reconnect script and guest journal. Only systemd is
// substituted; its retained unit identity is represented by a file.
func reconnectFixture(t *testing.T, state string) (Guest, guestjob.Request, string) {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("Python required for reconnect journal checks")
	}
	dir := t.TempDir()
	source, err := os.ReadFile("../guestjob/runner.py")
	if err != nil {
		t.Fatal(err)
	}
	root, _ := json.Marshal(dir)
	stub := string(source) + "\nROOT=pathlib.Path(" + string(root) + ")/'jobs'\nWORK_ROOT=pathlib.Path(" + string(root) + ").resolve()/'work'\n" + `
launched=ROOT.parent/'launched'
def fake_ctl(*args):
 value=(b'active' if launched.exists() else b'inactive') if any('ActiveState' in x for x in args) else (b'loaded' if launched.exists() else b'not-found')
 return subprocess.CompletedProcess(args,0,value,b'')
def fake_start(*args,**kwargs):
 launched.write_text(launched.read_text()+'x' if launched.exists() else 'x')
 return subprocess.CompletedProcess(args,0,b'',b'')
systemctl=fake_ctl
subprocess.run=fake_start
`
	runner := filepath.Join(dir, "runner.py")
	if err := os.WriteFile(runner, []byte(stub), 0600); err != nil {
		t.Fatal(err)
	}
	request := guestjob.Request{ID: "plugin_fixture", Args: []string{"python3", "main.py"}, Dir: filepath.Join(dir, "work", "revision"), Env: map[string]string{"ENVCTL_PLUGIN_STATE": "state"}, TimeoutSeconds: 30, Secrets: []string{"initial-synthetic"}, Input: `{"protocol":1,"operation":"prepare","operation_id":"one","run_id":"run_one","revision":"rev_one","config":{"setting":1},"credentials":{"token":"initial-synthetic"}}`}
	if err := os.MkdirAll(request.Dir, 0700); err != nil {
		t.Fatal(err)
	}
	if state != "missing" {
		raw, _ := json.Marshal(request)
		seed := `import importlib.util,json,sys
spec=importlib.util.spec_from_file_location('runner',sys.argv[1]);r=importlib.util.module_from_spec(spec);spec.loader.exec_module(r)
req=json.load(sys.stdin);directory=r.jobdir(req['id']);directory.mkdir(parents=True)
r.atomic(directory/'request.json',req)
digest=r.hashlib.sha256(json.dumps(req,sort_keys=True,separators=(',',':')).encode()).hexdigest()
r.atomic(directory/'receipt.json',{'digest':digest})
if sys.argv[2]=='completed':r.atomic(directory/'result.json',{'state':'completed','exit_code':0})
if sys.argv[2]=='interrupted':r.atomic(directory/'started.json',{'at':0})
`
		cmd := exec.Command(python, "-B", "-c", seed, runner, state)
		cmd.Stdin = bytes.NewReader(raw)
		if err := cmd.Run(); err != nil {
			t.Fatal("seed guest journal", err)
		}
	}
	return Guest{Client: guestjob.Client{Provider: reconnectExecutor{python, runner}, Runtime: "test"}}, request, dir
}

func TestPluginReconnectPreservesFrozenJournal(t *testing.T) {
	for _, state := range []string{"completed", "pending", "interrupted", "missing"} {
		t.Run(state, func(t *testing.T) {
			g, req, dir := reconnectFixture(t, state)
			path := filepath.Join(dir, "jobs", req.ID, "request.json")
			before, _ := os.ReadFile(path)
			req.Input = strings.ReplaceAll(req.Input, "initial-synthetic", "rotated-synthetic")
			req.Secrets = []string{"rotated-synthetic"}
			for i := 0; i < 2; i++ {
				exists, err := g.reconnect(context.Background(), req)
				if err != nil || exists != (state != "missing") {
					t.Fatal("reconnect state mismatch", err)
				}
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("reconnect rewrote frozen credentials")
			}
			launches, _ := os.ReadFile(filepath.Join(dir, "launched"))
			want := ""
			if state == "pending" {
				want = "x"
			}
			if string(launches) != want {
				t.Fatal("reconnect duplicated or restarted execution")
			}
		})
	}
}

func TestPluginReconnectRejectsChangedExecution(t *testing.T) {
	for name, change := range map[string]func(*guestjob.Request){
		"command":     func(r *guestjob.Request) { r.Args = []string{"another-command"} },
		"directory":   func(r *guestjob.Request) { r.Dir += "/another" },
		"environment": func(r *guestjob.Request) { r.Env["EXTRA"] = "value" },
		"timeout":     func(r *guestjob.Request) { r.TimeoutSeconds++ },
		"revision":    func(r *guestjob.Request) { r.Input = strings.ReplaceAll(r.Input, "rev_one", "rev_two") },
		"operation":   func(r *guestjob.Request) { r.Input = strings.ReplaceAll(r.Input, "prepare", "cleanup") },
		"config":      func(r *guestjob.Request) { r.Input = strings.ReplaceAll(r.Input, `"setting":1`, `"setting":true`) },
	} {
		t.Run(name, func(t *testing.T) {
			g, req, dir := reconnectFixture(t, "pending")
			change(&req)
			if _, err := g.reconnect(context.Background(), req); err == nil {
				t.Fatal("changed execution accepted")
			}
			if _, err := os.Stat(filepath.Join(dir, "launched")); !os.IsNotExist(err) {
				t.Fatal("conflicting reconnect launched a process")
			}
		})
	}
}

func TestPluginReconnectRejectsDamagedReceipt(t *testing.T) {
	g, req, dir := reconnectFixture(t, "completed")
	if err := os.WriteFile(filepath.Join(dir, "jobs", req.ID, "receipt.json"), []byte(`{"digest":"damaged"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := g.reconnect(context.Background(), req); err == nil {
		t.Fatal("damaged original receipt accepted")
	}
}
