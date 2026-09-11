package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestBundledBrowserIsFrozenAndSourceIntegrityChecked(t *testing.T) {
	c := workflow.Config{Plugins: []workflow.PluginRef{{ID: "playwright", Source: "builtin:playwright", Version: "1.0.0", Config: map[string]any{"base_url": "http://127.0.0.1:8080"}}}}
	cache := t.TempDir()
	frozen, lock, err := Freeze(cache, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Bindings) != 1 || lock.Capabilities["browser.test"] != "playwright" || frozen.Plugins[0].Digest == "" {
		t.Fatal("browser binding not resolved")
	}
	if _, err = os.Stat(filepath.Join(frozen.Plugins[0].Source, "package-lock.json")); err != nil {
		t.Fatal(err)
	}
	if c.Plugins[0].Source != "builtin:playwright" {
		t.Fatal("project source changed")
	}
	if _, _, err = Freeze(cache, c); err != nil {
		t.Fatal("repeated package retention", err)
	}
	ref := frozen.Plugins[0]
	ref.Version = "2.0.0"
	if _, err = Load("", ref); err == nil {
		t.Fatal("unavailable browser version accepted")
	}
}
