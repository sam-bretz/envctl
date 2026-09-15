package localexec

import (
	"context"
	"testing"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/publication"
	"github.com/sam-bretz/envctl/internal/runstore"
	"github.com/sam-bretz/envctl/internal/tracker"
)

type fakeTrackerProber struct {
	calls int
}

func (f *fakeTrackerProber) Probe(context.Context, engine.Assignment, string) (bool, string, error) {
	f.calls++
	return true, "tracker fixture", nil
}

func TestMultiProbeRoutesByCapabilityName(t *testing.T) {
	fake := &fakeTrackerProber{}
	m := multiProbe{publish: &publication.Broker{}, tracker: fake}
	ok, detail, err := m.Probe(context.Background(), engine.Assignment{}, "tracker.comment")
	if err != nil || !ok || detail != "tracker fixture" || fake.calls != 1 {
		t.Fatalf("tracker capability not routed to the tracker prober: %v %q %v", ok, detail, err)
	}
	if _, _, err = m.Probe(context.Background(), engine.Assignment{}, "publication.pr"); err != nil {
		t.Fatal(err)
	}
	if fake.calls != 1 {
		t.Fatal("publication.pr call reached the tracker prober")
	}
}

func TestNewWiresLinearProberForTrackerComment(t *testing.T) {
	store, err := runstore.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	b := New(store)
	m, ok := b.Capabilities.(multiProbe)
	if !ok {
		t.Fatalf("Backend.Capabilities is not a multiProbe: %T", b.Capabilities)
	}
	if _, ok := m.tracker.(tracker.LinearProber); !ok {
		t.Fatalf("tracker prober is not the Linear implementation: %T", m.tracker)
	}
}
