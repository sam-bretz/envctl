package engine

import (
	"context"
	"errors"
	"time"

	"github.com/sam-bretz/envctl/internal/workflow"
)

// PreviewBackend is optional. It keeps an explicit host-loopback forward to
// a runtime's configured preview service and returns its URL only while the
// service answers through it; "" means not (yet) reachable.
type PreviewBackend interface {
	Preview(context.Context, Assignment) (string, error)
}

const previewInterval = 10 * time.Second

// schedulePreviews reconciles previews for every ready runtime of a revision,
// including completed ones kept for inspection. A restarted coordinator
// re-establishes forwards on its first pass.
func (e *Engine) schedulePreviews(ctx context.Context, run workflow.Run, rev workflow.Revision) {
	if _, ok := e.Backend.(PreviewBackend); !ok || rev.Config.Preview == nil {
		return
	}
	targets := []string{}
	if rev.Runtime.Ready {
		targets = append(targets, "")
	}
	for node, child := range rev.ChildRuntimes {
		if child != nil && child.Runtime.Ready {
			targets = append(targets, node)
		}
	}
	for _, child := range targets {
		key := run.ID + "/" + rev.ID + "/preview/" + child
		if !e.previewDue(key) {
			continue
		}
		e.launch(ctx, key, func() error { return e.reconcilePreview(ctx, run.ID, rev.ID, child) })
	}
}

func (e *Engine) previewDue(key string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.previewAt == nil {
		e.previewAt = map[string]time.Time{}
	}
	if last, ok := e.previewAt[key]; ok && e.now().Sub(last) < previewInterval {
		return false
	}
	e.previewAt[key] = e.now()
	return true
}

func (e *Engine) reconcilePreview(ctx context.Context, runID, revision, child string) error {
	backend := e.Backend.(PreviewBackend)
	run, err := e.Store.Get(ctx, runID)
	if err != nil {
		return err
	}
	rev := run.Revision(revision)
	if rev == nil {
		return nil
	}
	a := assign(run, rev, nil)
	if child != "" {
		if rev.ChildRuntimes[child] == nil {
			return nil
		}
		a = childAssignment(a, child)
	}
	if !a.Revision.Runtime.Ready {
		return nil
	}
	url, err := backend.Preview(ctx, a)
	if err != nil {
		return err
	}
	if url == a.Revision.Runtime.PreviewURL {
		return nil
	}
	_, err = e.update(ctx, runID, revision, "runtime.preview", func(_ *workflow.Run, v *workflow.Revision) error {
		rt := &v.Runtime
		if child != "" {
			c := v.ChildRuntimes[child]
			if c == nil {
				return workflow.ErrConflict
			}
			rt = &c.Runtime
		}
		if rt.ID != a.Revision.Runtime.ID || !rt.Ready {
			return workflow.ErrConflict
		}
		rt.PreviewURL = url
		return nil
	})
	if errors.Is(err, workflow.ErrConflict) {
		return nil
	}
	return err
}
