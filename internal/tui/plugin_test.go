package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPluginAttachmentAndRemovalAreRevisionScopedActions(t *testing.T) {
	m := modelFixture(t)
	dir := t.TempDir()
	ref := filepath.Join(dir, "browser.yaml")
	if err := os.WriteFile(ref, []byte("id: browser\nsource: packages/browser\nversion: 1.0.0\nprovides: [browser.check]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	api := m.API.(*fakeAPI)

	m, _ = key(m, "p")
	m.Input = filepath.Join(dir, "missing.yaml")
	m, cmd := key(m, "enter")
	if cmd != nil || m.Error == "" || api.request.Action != "" {
		t.Fatal("unreadable plugin reference was submitted")
	}

	m.Error = ""
	m, _ = key(m, "p")
	m.Input = ref
	m, cmd = key(m, "enter")
	if cmd == nil {
		t.Fatal("no attachment command")
	}
	cmd()
	if api.request.Action != "plugin-attach" || api.request.Plugin == nil || api.request.Plugin.ID != "browser" || api.request.Plugin.Source != filepath.Join(dir, "packages/browser") {
		t.Fatalf("attachment request %+v", api.request)
	}
	if api.request.Revision != m.Runs[0].CurrentRevision || api.request.ExpectedVersion != m.Runs[0].Version {
		t.Fatal("attachment is not bound to the viewed revision and version")
	}

	m, _ = key(m, "P")
	m.Input = "browser"
	m, cmd = key(m, "enter")
	if cmd == nil {
		t.Fatal("no removal command")
	}
	cmd()
	if api.request.Action != "plugin-remove" || api.request.PluginID != "browser" || api.request.Revision != m.Runs[0].CurrentRevision {
		t.Fatalf("removal request %+v", api.request)
	}
}
