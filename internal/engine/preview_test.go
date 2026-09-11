package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

type previewFixture struct {
	*fixtureBackend
	mu    sync.Mutex
	url   string
	calls int
}

func (p *previewFixture) Preview(context.Context, Assignment) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	return p.url, nil
}

func TestPreviewURLIsReconciledForCompletedRuntimesAndClearedWhenUnreachable(t *testing.T) {
	h := setup(t)
	h.mutate(func(r *workflow.Run) error {
		v := r.Current()
		v.Config.Preview = &workflow.Preview{Service: "web", Port: 8080}
		v.State = "completed"
		v.Runtime.Ready, v.Runtime.State = true, "running"
		return nil
	})
	p := &previewFixture{fixtureBackend: h.backend, url: "http://127.0.0.1:41234/"}
	h.engine.Backend = p
	ctx := context.Background()

	// A completed revision keeps its runtime for inspection, so Tick still
	// reconciles its preview (as a restarted coordinator would).
	if err := h.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.wg.Wait()
	run := h.run()
	if run.Current().Runtime.PreviewURL != p.url {
		t.Fatal("preview URL not recorded", run.Current().Runtime)
	}
	version := run.Version
	// Unchanged URL: no write. Throttled: a second Tick inside the interval
	// does not call the backend again.
	calls := p.calls
	if err := h.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.wg.Wait()
	if p.calls != calls || h.run().Version != version {
		t.Fatal("preview reconciliation was not throttled or rewrote an unchanged URL")
	}
	if err := h.engine.reconcilePreview(ctx, h.id, h.rev, ""); err != nil || h.run().Version != version {
		t.Fatal("unchanged preview URL was rewritten", err)
	}
	// The backend reports an unreachable preview: the URL is withdrawn.
	p.url = ""
	h.now = h.now.Add(11 * time.Second)
	if err := h.engine.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	h.engine.wg.Wait()
	if got := h.run().Current().Runtime.PreviewURL; got != "" {
		t.Fatal("unreachable preview still advertised", got)
	}
}

func TestPreviewIsNotReconciledWithoutConfigurationOrReadyRuntime(t *testing.T) {
	h := setup(t)
	p := &previewFixture{fixtureBackend: h.backend, url: "http://127.0.0.1:41234/"}
	h.engine.Backend = p
	h.engine.schedulePreviews(context.Background(), *h.run(), *h.run().Current())
	h.engine.wg.Wait()
	if p.calls != 0 {
		t.Fatal("preview reconciled without configuration")
	}
	h.mutate(func(r *workflow.Run) error {
		r.Current().Config.Preview = &workflow.Preview{Service: "web", Port: 8080}
		return nil
	})
	h.engine.schedulePreviews(context.Background(), *h.run(), *h.run().Current())
	h.engine.wg.Wait()
	if p.calls != 0 {
		t.Fatal("preview reconciled for a runtime that is not ready")
	}
}
