package daemon

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func TestPluginAttachmentFreezesPackageAndCreatesIsolatedRevision(t *testing.T) {
	c, store := testAPI(t)
	r := create(t, c)
	ctx := context.Background()
	// A second invocation must not inherit the first invocation's attachment.
	other, err := c.Create(ctx, CreateRequest{OperationID: "other", Task: "Independent", Name: "Other", Config: r.Current().Config})
	if err != nil {
		t.Fatal(err)
	}
	r, err = store.Mutate(ctx, r.ID, r.Version, "running", "fixture", nil, func(r *workflow.Run) error {
		r.Current().State = "active"
		r.Current().Attempts = []workflow.Attempt{{ID: "attempt_code", Node: "code", State: "running", Number: 1}}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	desc := plugin.Descriptor{ID: "browser", Version: "1.0.0", Protocol: 1, Provides: []string{"browser.test"}, Requires: []string{"runtime.compose"}, Command: []string{"python3", "main.py"}, Operations: []string{"probe"}}
	raw, _ := json.Marshal(desc)
	if err = os.WriteFile(filepath.Join(dir, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "main.py"), []byte("print('fixture')"), 0600); err != nil {
		t.Fatal(err)
	}
	ref := workflow.PluginRef{ID: "browser", Version: "1.0.0", Source: dir}
	req := ActionRequest{OperationID: "attach", ExpectedVersion: r.Version, Revision: r.CurrentRevision, Action: "plugin-attach", Plugin: &ref}
	next, err := c.Action(ctx, r.ID, req)
	if err != nil {
		t.Fatal(err)
	}
	if next.CurrentRevision == r.CurrentRevision || next.Revision(r.CurrentRevision).State != "draining" || len(next.Revision(r.CurrentRevision).Config.Plugins) != 0 {
		t.Fatal("live revision mutated")
	}
	if len(next.Current().Config.Plugins) != 1 || next.Current().Config.Plugins[0].Digest == "" || len(next.Current().ReadinessProblems(time.Now(), next.Current().Requirements())) == 0 {
		t.Fatal("attachment bypassed Plan")
	}
	frozen := next.Current().Config.Plugins[0]
	if frozen.Source == dir {
		t.Fatal("source not retained")
	}
	if err = os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	replay, err := c.Action(ctx, r.ID, req)
	if err != nil || replay.CurrentRevision != next.CurrentRevision {
		t.Fatal("lost response duplicated attachment or reread deleted source", err)
	}
	if _, err = plugin.Load("", frozen); err != nil {
		t.Fatal(err)
	}
	reattached, err := c.Action(ctx, r.ID, ActionRequest{OperationID: "same-binding", ExpectedVersion: next.Version, Revision: next.CurrentRevision, Action: "plugin-attach", Plugin: &frozen})
	if err != nil || reattached.CurrentRevision != next.CurrentRevision {
		t.Fatal("identical binding reopened Plan", err)
	}
	next = reattached
	other, err = c.Get(ctx, other.ID)
	if err != nil || len(other.Current().Config.Plugins) != 0 {
		t.Fatal("attachment escaped invocation", err)
	}
	removed, err := c.Action(ctx, r.ID, ActionRequest{OperationID: "remove", ExpectedVersion: next.Version, Revision: next.CurrentRevision, Action: "plugin-remove", PluginID: "browser"})
	if err != nil || len(removed.Current().Config.Plugins) != 0 || len(removed.Revision(next.CurrentRevision).Config.Plugins) != 1 {
		t.Fatal("removal mutated historical revision", err)
	}
}
