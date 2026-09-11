package plugin

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestFreezeRetainsInvocationPackageWithoutMutatingSource(t *testing.T) {
	b := fixture(t)
	c := workflow.Config{Dir: b.SourceDir, Plugins: []workflow.PluginRef{b.Ref}}
	frozen, lock, err := Freeze(t.TempDir(), c)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.Plugins[0].Source == c.Plugins[0].Source || len(lock.Bindings) != 1 {
		t.Fatal("package was not retained independently")
	}
	if len(c.Plugins[0].Provides) != 0 {
		t.Fatal("project default mutated")
	}
	if err = os.RemoveAll(b.SourceDir); err != nil {
		t.Fatal(err)
	}
	retained, err := Load("", frozen.Plugins[0])
	if err != nil || retained.Digest != b.Digest {
		t.Fatal("original deletion invalidated retained invocation", err)
	}
	if err = os.WriteFile(filepath.Join(retained.SourceDir, "plugin.sh"), []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Freeze(t.TempDir(), frozen); err == nil {
		t.Fatal("tampered retained package admitted")
	}
}

func TestPluginRefPathsResolveAgainstAttachmentFile(t *testing.T) {
	dir := t.TempDir()
	ref, err := decodeRef([]byte("id: fixture\nversion: 1.0.0\nsource: ./package\ncredentials: {token: 'file:./secret'}\n"), dir)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Source != filepath.Join(dir, "package") || ref.Credentials["token"] != "file:"+filepath.Join(dir, "secret") {
		t.Fatal("attachment paths not scoped to file")
	}
}
