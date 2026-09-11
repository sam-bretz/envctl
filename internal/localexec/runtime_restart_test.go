package localexec

import (
	"context"
	"testing"

	vm "github.com/sam-bretz/envctl/internal/runtime"
)

type restartFixture struct {
	vm.Provider
	state, daemon string
	missing       bool
	ensured       []vm.Spec
}

func (f *restartFixture) Inspect(_ context.Context, id string) (vm.Instance, error) {
	if f.missing {
		return vm.Instance{}, vm.ErrMissing
	}
	return vm.Instance{ID: id, State: f.state}, nil
}
func (f *restartFixture) Ensure(_ context.Context, s vm.Spec) (vm.Instance, error) {
	f.ensured = append(f.ensured, s)
	f.state = "running"
	return vm.Instance{ID: s.ID, State: "running", DaemonID: f.daemon}, nil
}

func TestStoppedRuntimeRestartsUnderItsReservationWithTheSameDaemon(t *testing.T) {
	b, a := fixture(t)
	a.Revision.Runtime.DaemonID = "daemon-a"
	a.Revision.Config.Runtime.MemoryGiB = 3
	f := &restartFixture{state: "running", daemon: "daemon-a"}
	b.Provider = f
	ctx := context.Background()
	if err := b.ensureRuntime(ctx, a); err != nil || len(f.ensured) != 0 {
		t.Fatal("running VM was touched", err, f.ensured)
	}
	f.state = "stopped"
	if err := b.ensureRuntime(ctx, a); err != nil {
		t.Fatal(err)
	}
	if len(f.ensured) != 1 || f.ensured[0].ID != a.Revision.Runtime.ID || f.ensured[0].MemoryGiB != 3 {
		t.Fatal("restart did not reuse the reserved runtime spec", f.ensured)
	}
	f.state, f.daemon = "stopped", "daemon-b"
	if err := b.ensureRuntime(ctx, a); err == nil {
		t.Fatal("changed Docker daemon identity accepted after restart")
	}
	f.missing = true
	if err := b.ensureRuntime(ctx, a); err == nil || len(f.ensured) != 2 {
		t.Fatal("missing VM must be explicit recovery work, never recreated", err)
	}
}
