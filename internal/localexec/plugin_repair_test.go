package localexec

import (
	"context"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/plugin"
	"github.com/sam-bretz/envctl/internal/workflow"
)

func expirePluginProbe(t *testing.T, b *Backend, a engine.Assignment, binding plugin.Binding) {
	t.Helper()
	record, err := b.readPluginOperation(a, binding, "probe")
	if err != nil {
		t.Fatal(err)
	}
	record.CompletedAt = time.Now().Add(-time.Hour)
	filename, _ := b.pluginPath(a, binding, "probe")
	if err := atomicJSON(filename, record); err != nil {
		t.Fatal(err)
	}
}

func releasePluginRepairBackoff(t *testing.T, b *Backend, a engine.Assignment, binding plugin.Binding) pluginRepair {
	t.Helper()
	repair, filename, err := b.readPluginRepair(a, binding)
	if err != nil || repair.Trigger == "" {
		t.Fatal("missing durable repair intent", err)
	}
	repair.RetryAt = time.Now().Add(-time.Second)
	if err := atomicJSON(filename, repair); err != nil {
		t.Fatal(err)
	}
	return repair
}

func TestPluginResourceRepairReconnectAndFreshVerification(t *testing.T) {
	for _, operation := range []string{"prepare", "renew"} {
		t.Run(operation, func(t *testing.T) {
			b, a, binding, p := pluginFixture(t, "prepare", "renew")
			resource, preparations, renewals, verifications := false, 0, 0, 0
			loseRepair, loseVerification := false, false
			p.respond = func(r plugin.Request) plugin.Response {
				response := plugin.Response{Protocol: 1, OK: true, Detail: "resource verified"}
				switch r.Operation {
				case "prepare", "renew":
					if r.Operation == "prepare" {
						preparations++
					} else {
						renewals++
					}
					resource = true
					if loseRepair {
						p.losePoll, loseRepair = true, false
					}
				case "probe":
					verifications++
					response.OK = resource
					if !resource {
						response.Recovery = operation
					}
					if loseVerification {
						p.losePoll, loseVerification = true, false
					}
				}
				return response
			}
			check := func(want bool) {
				probes, err := b.pluginProbes(context.Background(), a, []workflow.Probe{{Capability: "fixture.ready"}})
				if err != nil || probes[0].Passed != want {
					t.Fatalf("readiness=%v want %v: %v", probes, want, err)
				}
			}
			check(true)
			resource = false
			expirePluginProbe(t, b, a, binding)
			check(false)
			intent := releasePluginRepairBackoff(t, b, a, binding)
			if preparations != 1 || renewals != 0 || intent.Operation != operation {
				t.Fatal("failed probe dispatched repair before durable intent")
			}
			loseRepair = true
			check(false) // effect succeeded; observation was lost
			b = New(b.Store)
			b.Provider = p
			loseVerification = true
			check(false) // repair reconnected; fresh probe acknowledgement lost
			b = New(b.Store)
			b.Provider = p
			check(true)
			check(true)
			wantPrepare, wantRenew := 2, 0
			if operation == "renew" {
				wantPrepare, wantRenew = 1, 1
			}
			if preparations != wantPrepare || renewals != wantRenew || verifications != 3 {
				t.Fatalf("reconnect duplicated work: prepare=%d renew=%d probes=%d", preparations, renewals, verifications)
			}
			if repair, _, err := b.readPluginRepair(a, binding); err != nil || !repair.Complete || repair.Count != 0 {
				t.Fatal("successful verification did not finish incident", err)
			}
		})
	}
}

func TestPluginResourceRepairCannotPassWithoutVerificationOrEscapeBudget(t *testing.T) {
	b, a, binding, p := pluginFixture(t, "prepare")
	a.Revision.Config.Limits.MaxAttempts = 2
	preparations := 0
	p.respond = func(r plugin.Request) plugin.Response {
		if r.Operation == "prepare" {
			preparations++
			return plugin.Response{Protocol: 1, OK: true, Detail: "prepared"}
		}
		return plugin.Response{Protocol: 1, OK: false, Detail: "resource still missing", Recovery: "prepare"}
	}
	for i := 0; i < 5; i++ {
		probes, err := b.pluginProbes(context.Background(), a, []workflow.Probe{{Capability: "fixture.ready"}})
		if err != nil || probes[0].Passed {
			t.Fatal("repair result admitted without passing verification", err)
		}
		releasePluginRepairBackoff(t, b, a, binding)
		b = New(b.Store)
		b.Provider = p
	}
	if preparations != 3 { // initial preparation plus two recovery cycles
		t.Fatalf("repair budget lost across recreation: %d prepares", preparations)
	}
}

func TestPluginFailedProbeWithoutRepairRequestDoesNotReprepare(t *testing.T) {
	b, a, binding, p := pluginFixture(t, "prepare")
	preparations := 0
	p.respond = func(r plugin.Request) plugin.Response {
		if r.Operation == "prepare" {
			preparations++
			return plugin.Response{Protocol: 1, OK: true}
		}
		return plugin.Response{Protocol: 1, OK: false, Detail: "connection unavailable"}
	}
	for i := 0; i < 3; i++ {
		probes, err := b.pluginProbes(context.Background(), a, []workflow.Probe{{Capability: "fixture.ready"}})
		if err != nil || probes[0].Passed {
			t.Fatal("failed probe admitted", err)
		}
		expirePluginProbe(t, b, a, binding)
	}
	if preparations != 1 {
		t.Fatal("unrequested resource mutation")
	}
}
