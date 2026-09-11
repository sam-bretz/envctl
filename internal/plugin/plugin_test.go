package plugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/workflow"
)

func fixture(t *testing.T) Binding {
	t.Helper()
	d := t.TempDir()
	desc := Descriptor{ID: "fixture", Version: "1.0.0", Protocol: 1, Provides: []string{"fixture.ready"}, Command: []string{"sh", "plugin.sh"}, Operations: []string{"prepare", "probe", "execute", "renew", "cleanup"}}
	raw, _ := json.Marshal(desc)
	if err := os.WriteFile(filepath.Join(d, "plugin.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "plugin.sh"), []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"protocol\":1,\"ok\":true,\"detail\":\"ready\",\"expires_seconds\":60}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	b, err := Load(d, workflow.PluginRef{ID: "fixture", Source: d, Version: "1.0.0"})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestResolveTransitiveCapabilitiesAndRejectConflicts(t *testing.T) {
	a := fixture(t)
	b := a
	b.Ref.ID = "dependent"
	b.Descriptor.Provides = []string{"dependent.ready"}
	b.Descriptor.Requires = []string{"fixture.ready"}
	lock, err := Resolve([]Binding{b, a}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(lock.Bindings) != 2 || lock.Bindings[0].Ref.ID != "fixture" {
		t.Fatal("dependency order", lock)
	}
	if _, err = Resolve([]Binding{a, b}, map[string]string{"fixture.ready": "builtin"}); err == nil {
		t.Fatal("conflicting capability accepted")
	}
	a.Descriptor.Requires = []string{"dependent.ready"}
	if _, err = Resolve([]Binding{a, b}, nil); err == nil {
		t.Fatal("cycle accepted")
	}
	b.Descriptor.Requires = []string{"missing.ready"}
	if _, err = Resolve([]Binding{b}, nil); err == nil {
		t.Fatal("missing capability accepted")
	}
}
func TestExecutableProtocolAndSourceIntegrity(t *testing.T) {
	b := fixture(t)
	execute := func(ctx context.Context, b Binding, input []byte) ([]byte, []byte, error) {
		cmd := exec.CommandContext(ctx, b.Descriptor.Command[0], b.Descriptor.Command[1:]...)
		cmd.Dir = b.SourceDir
		cmd.Stdin = bytes.NewReader(input)
		var errout bytes.Buffer
		cmd.Stderr = &errout
		out, err := cmd.Output()
		return out, errout.Bytes(), err
	}
	req := Request{Protocol: 1, Operation: "probe", OperationID: "probe-one"}
	res, evidence, err := Call(context.Background(), execute, b, req)
	if err != nil || !res.OK || len(evidence) == 0 {
		t.Fatalf("probe: %+v %v", res, err)
	}
	if err = os.WriteFile(filepath.Join(b.SourceDir, "plugin.sh"), []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Call(context.Background(), execute, b, req); err == nil {
		t.Fatal("changed plugin executed")
	}
}
func TestSecretsRedactedFromEvidenceAndErrors(t *testing.T) {
	b := fixture(t)
	secret := `abc"123\x`
	req := Request{Protocol: 1, Operation: "probe", OperationID: "probe", Credentials: map[string]string{"TOKEN": secret}}
	execute := func(_ context.Context, _ Binding, input []byte) ([]byte, []byte, error) {
		if !bytes.Contains(input, []byte(`credentials`)) {
			t.Fatal("credential not delivered on stdin")
		}
		out, _ := json.Marshal(Response{Protocol: 1, OK: true, Detail: "token=" + secret, Output: map[string]any{"nested": []any{secret}}})
		return out, nil, nil
	}
	res, evidence, err := Call(context.Background(), execute, b, req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Detail, secret) || !strings.Contains(string(evidence), "[redacted]") {
		t.Fatal("credential leaked")
	}
	fail := func(context.Context, Binding, []byte) ([]byte, []byte, error) {
		return nil, []byte(secret), os.ErrPermission
	}
	_, _, err = Call(context.Background(), fail, b, req)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatal("stderr leaked credential")
	}
}
func TestDescriptorVersionAndSymlinkPolicy(t *testing.T) {
	b := fixture(t)
	ref := b.Ref
	ref.Version = "2"
	if _, err := Load(b.SourceDir, ref); err == nil {
		t.Fatal("version mismatch accepted")
	}
	if err := os.Symlink("/etc/passwd", filepath.Join(b.SourceDir, "outside")); err != nil {
		t.Fatal(err)
	}
	if _, err := SourceDigest(b.SourceDir); err == nil {
		t.Fatal("external source link accepted")
	}
}
func TestMalformedResponsesDoNotLeakRawOutput(t *testing.T) {
	b := fixture(t)
	execute := func(context.Context, Binding, []byte) ([]byte, []byte, error) {
		return []byte("secret-token invalid json"), nil, nil
	}
	_, _, err := Call(context.Background(), execute, b, Request{Protocol: 1, Operation: "probe", OperationID: "p"})
	if err == nil || strings.Contains(err.Error(), "secret-token") {
		t.Fatal("invalid response exposed raw output")
	}
}

func TestOnlyKnownTerminalOutcomesPermitRecovery(t *testing.T) {
	b := fixture(t)
	req := Request{Protocol: 1, Operation: "probe", OperationID: "probe", Credentials: map[string]string{"token": "private-value"}}
	for _, tc := range []struct {
		name, raw string
		err       error
		terminal  bool
	}{
		{"transport", "", errors.New("connection reset"), false},
		{"process", "", &TerminalFailure{Detail: "failed private-value"}, true},
		{"protocol", "private-value invalid JSON", nil, true},
		{"version", `{"protocol":99,"ok":true,"detail":"private-value"}`, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := Call(context.Background(), func(context.Context, Binding, []byte) ([]byte, []byte, error) {
				return []byte(tc.raw), []byte("private-value"), tc.err
			}, b, req)
			var terminal *TerminalFailure
			if err == nil || errors.As(err, &terminal) != tc.terminal || strings.Contains(err.Error(), "private-value") {
				t.Fatal("unsafe failure classification or redaction", err)
			}
		})
	}
}

func TestRecoveryRequestsRequireFailedProbeAndDeclaredLifecycle(t *testing.T) {
	for _, tc := range []struct {
		name, operation, recovery string
		ok, declared, valid       bool
	}{
		{"prepare", "probe", "prepare", false, true, true},
		{"renew", "probe", "renew", false, true, true},
		{"unknown", "probe", "private-value", false, true, false},
		{"undeclared", "probe", "renew", false, false, false},
		{"passed", "probe", "prepare", true, true, false},
		{"execute", "execute", "prepare", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := fixture(t)
			b.Descriptor.Operations = []string{"probe", "execute"}
			if tc.declared {
				b.Descriptor.Operations = append(b.Descriptor.Operations, "prepare", "renew")
			}
			_, _, err := Call(context.Background(), func(context.Context, Binding, []byte) ([]byte, []byte, error) {
				raw, _ := json.Marshal(Response{Protocol: 1, OK: tc.ok, Recovery: tc.recovery})
				return raw, nil, nil
			}, b, Request{Protocol: 1, Operation: tc.operation, OperationID: "p"})
			if (err == nil) != tc.valid {
				t.Fatal("unexpected recovery validation", err)
			}
			if err != nil {
				var terminal *TerminalFailure
				if !errors.As(err, &terminal) || strings.Contains(err.Error(), "private-value") {
					t.Fatal("invalid recovery leaked output or did not terminate protocol", err)
				}
			}
		})
	}
}
