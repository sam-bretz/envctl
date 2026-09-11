package localexec

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	_ "image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/plugin"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestRealBrowserCheckPlugin(t *testing.T) {
	if os.Getenv("ENVCTL_BROWSER_TEST") != "1" {
		t.Skip("opt-in real Chromium plugin in the owned guest")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("explicit owned guest required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	b, a := fixture(t)
	a.Run.ID, a.Revision.ID, a.Attempt.ID = workflow.ID("run"), workflow.ID("rev"), workflow.ID("attempt")
	b.Provider = vm.NewLima(state)
	a.Revision.Runtime.ID = name
	root := "/work/envctl/fixtures/" + a.Revision.ID
	if err := b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "mkdir", "-p", root}}); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"compose.yaml": `services:
  web:
    image: busybox:1.37.0
    command: [httpd, -f, -p, '8080', -h, /www]
    volumes: ['./index.html:/www/index.html:ro']
    ports: ['19091:8080']
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']
      interval: 1s
      timeout: 1s
      retries: 20
`, "index.html": `<!doctype html><title>Browser acceptance</title><h1>Addition</h1><input id="left"><input id="right"><button id="add">Add</button><output id="result"></output><script>document.querySelector('#add').onclick=()=>{document.querySelector('#result').textContent=String(Number(document.querySelector('#left').value)+Number(document.querySelector('#right').value))}</script>`}
	for file, body := range files {
		if err := b.Provider.Exec(ctx, name, vm.Command{Args: []string{"sudo", "tee", root + "/" + file}, Stdin: strings.NewReader(body)}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := b.stack(a).Prepare(ctx, gueststack.Spec{ID: a.Revision.ID, Project: a.Revision.ID, Root: root, Stack: manifest.Stack{Files: []string{"compose.yaml"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := b.cleanupPlugins(clean, a); err != nil {
			t.Error("browser cleanup", err)
		}
		if err := b.stack(a).Down(clean, p, true); err != nil {
			t.Error(err)
		}
		if err := b.Provider.Exec(clean, name, vm.Command{Args: []string{"sudo", "rm", "-rf", "--", root, "/work/envctl/plugins/" + a.Revision.ID}}); err != nil {
			t.Error(err)
		}
	})
	if err = b.stack(a).Up(ctx, p, 60); err != nil {
		t.Fatal(err)
	}
	check := workflow.Check{Name: "browser-addition", Plugin: "playwright", TimeoutSeconds: 120, Input: map[string]any{"steps": []any{map[string]any{"fill": map[string]any{"selector": "#left", "value": "2"}}, map[string]any{"fill": map[string]any{"selector": "#right", "value": "3"}}, map[string]any{"click": "#add"}, map[string]any{"text": map[string]any{"selector": "#result", "equals": "5"}}}}}
	qa := a.Revision.Config.Workflow.Nodes["qa"]
	qa.Checks = []workflow.Check{check}
	qa.Requires = []string{"browser.test"}
	a.Revision.Config.Workflow.Nodes["qa"] = qa
	if ready, _ := pluginChecksReady(a); ready {
		t.Fatal("missing browser plugin admitted")
	}
	a.Revision.Config.Plugins = []workflow.PluginRef{{ID: "playwright", Version: "1.0.0", Source: "builtin:playwright", Config: map[string]any{"base_url": "http://127.0.0.1:19091"}}}
	c, lock, err := plugin.Freeze(filepath.Join(b.Store.Dir, "plugins"), a.Revision.Config)
	if err != nil {
		t.Fatal(err)
	}
	a.Revision.Config = c
	probes, err := b.pluginProbes(ctx, a, []workflow.Probe{{Capability: "runtime.compose", Passed: true, ExpiresAt: time.Now().Add(time.Hour)}, {Capability: "browser.test"}})
	if err != nil {
		t.Fatal("real Chromium readiness", err)
	}
	if !probes[1].Passed {
		t.Fatal("real Chromium readiness", probes[1].Detail)
	}
	if ready, _ := pluginChecksReady(a); !ready {
		t.Fatal("prepared browser check binding missing")
	}
	record, err := b.pluginCheck(ctx, a, check, 0)
	if err != nil {
		t.Fatal("browser interaction check", err)
	}
	if record.Response == nil || !record.Response.OK {
		t.Fatal("browser interaction check did not pass")
	}
	if record.Response.Output["title"] != "Browser acceptance" {
		t.Fatal("browser did not navigate to the real application")
	}
	encoded, ok := record.Response.Output["screenshot_png"].(string)
	if !ok {
		t.Fatal("browser check did not return screenshot evidence")
	}
	png, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatal(err)
	}
	dimensions, _, err := image.DecodeConfig(bytes.NewReader(png))
	if err != nil || dimensions.Width != 1024 || dimensions.Height != 640 {
		t.Fatal("invalid browser screenshot", err)
	}
	b = New(b.Store)
	b.Provider = vm.NewLima(state)
	replay, err := b.pluginCheck(ctx, a, check, 0)
	if err != nil || replay.ID != record.ID || replay.Evidence.Digest != record.Evidence.Digest {
		t.Fatal("browser check replay changed", err)
	}
	bad := check
	bad.Input = map[string]any{"steps": []any{map[string]any{"text": map[string]any{"selector": "#result", "equals": "wrong"}}}}
	failed, err := b.pluginCheck(ctx, a, bad, 1)
	if err != nil || failed.Response == nil || failed.Response.OK {
		t.Fatal("failed browser assertion was accepted", err)
	}
	if failed.Evidence.Digest == "" {
		t.Fatal("failed browser check lost evidence")
	}
	// Remove only this invocation's tools to simulate lost prepared resources.
	// Use a separate operation key so final lifecycle cleanup still runs after
	// repair recreates the volume and containers.
	binding := lock.Bindings[0]
	removed, err := b.callPluginInput(ctx, a, binding, "cleanup", "fixture_remove", nil, true)
	if err != nil || removed.Response == nil || !removed.Response.OK {
		t.Fatal("remove browser fixture resources", err)
	}
	expirePluginProbe(t, b, a, binding)
	probes, err = b.pluginProbes(ctx, a, []workflow.Probe{{Capability: "runtime.compose", Passed: true, ExpiresAt: time.Now().Add(time.Hour)}, {Capability: "browser.test"}})
	if err != nil || probes[1].Passed {
		t.Fatal("missing browser resources remained ready", err)
	}
	repair, _, err := b.readPluginRepair(a, binding)
	if err != nil || repair.Operation != "prepare" {
		t.Fatal("missing browser repair request", err)
	}
	b = New(b.Store)
	b.Provider = vm.NewLima(state)
	timer := time.NewTimer(time.Until(repair.RetryAt) + 20*time.Millisecond)
	select {
	case <-ctx.Done():
		timer.Stop()
		t.Fatal(ctx.Err())
	case <-timer.C:
	}
	probes, err = b.pluginProbes(ctx, a, []workflow.Probe{{Capability: "runtime.compose", Passed: true, ExpiresAt: time.Now().Add(time.Hour)}, {Capability: "browser.test"}})
	if err != nil || !probes[1].Passed {
		t.Fatal("browser resource repair failed", err, probes)
	}
	repairedCheck, err := b.pluginCheck(ctx, a, check, 2)
	if err != nil || repairedCheck.Response == nil || !repairedCheck.Response.OK {
		t.Fatal("repaired Chromium could not execute fresh application check", err)
	}
	t.Log("real Chromium readiness/interactions, assertion failure, screenshot evidence, restart replay, missing-tools repair and fresh application check passed")
}
