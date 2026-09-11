package runtime

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestRenderedGuestHasPinnedImageAndNoHostSharing(t *testing.T) {
	raw, err := RenderLima(DefaultSpec("envctl-test"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err = yaml.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if len(m["mounts"].([]any)) != 0 {
		t.Fatal("host mounts enabled")
	}
	images := m["images"].([]any)
	if len(images) != 1 || !strings.HasPrefix(images[0].(map[string]any)["digest"].(string), "sha256:") {
		t.Fatal("mutable image fallback")
	}
	if m["ssh"].(map[string]any)["forwardAgent"] != false {
		t.Fatal("host SSH agent forwarded")
	}
	if bytes.Contains(raw, []byte("guestSocket")) {
		t.Fatal("Docker socket exposed")
	}
	if m["portForwards"].([]any)[0].(map[string]any)["ignore"] != true {
		t.Fatal("implicit port exposure")
	}
	if m["portForwards"].([]any)[0].(map[string]any)["proto"] != "any" {
		t.Fatal("UDP forwarding was not disabled")
	}
	if m["portForwards"].([]any)[0].(map[string]any)["guestIP"] != "0.0.0.0" {
		t.Fatal("forwarding rule must match every guest interface")
	}
	if m["portForwards"].([]any)[0].(map[string]any)["guestIPMustBeZero"] != false {
		t.Fatal("forwarding exclusion must also match loopback listeners")
	}
}
func TestInvalidRuntimeSpecsRejected(t *testing.T) {
	for _, mutate := range []func(*Spec){func(s *Spec) { s.ID = "default" }, func(s *Spec) { s.ImageDigest = "" }, func(s *Spec) { s.Image = "http://example.com/image" }, func(s *Spec) { s.MemoryGiB = 0 }} {
		s := DefaultSpec("envctl-test")
		mutate(&s)
		if _, err := RenderLima(s); err == nil {
			t.Fatal("invalid spec accepted", s)
		}
	}
}
func TestLimaTemplateValidates(t *testing.T) {
	binary, err := exec.LookPath("limactl")
	if err != nil {
		t.Skip("Lima not installed")
	}
	raw, err := RenderLima(DefaultSpec("envctl-template-check"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "lima.yaml")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(binary, "validate", path).CombinedOutput()
	if err != nil {
		t.Fatalf("template invalid: %v\n%s", err, b)
	}
}

func TestEnsureRepairsIncompleteBootstrapWithoutAnotherVM(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "fake-limactl")
	// A boot-complete VM initially lacks Docker. The first repair fails, and
	// the next Ensure must reuse that VM and replay only idempotent bootstrap.
	script := `#!/bin/sh
set -eu
root=$(dirname "$0")
case "$1" in
 list) if [ -f "$root/created" ]; then printf '{"name":"envctl-repair","status":"Running"}\n'; fi;;
 start) printf x >> "$root/created";;
 shell)
  case "$5" in
   sh) test -f "$root/ready";;
   bash)
    cat >/dev/null
    if [ ! -f "$root/failed-once" ]; then touch "$root/failed-once"; echo 'package repository temporarily unavailable' >&2; exit 1; fi
    touch "$root/ready";;
   docker)
    case "$6" in
     info) echo dedicated-daemon;;
     version) echo 29.1.3;;
     compose) echo 2.40.3;;
    esac;;
  esac;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	l := NewLima(dir)
	l.Binary = binary
	spec := DefaultSpec("envctl-repair")
	if _, err := l.Ensure(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "package repository temporarily unavailable") {
		t.Fatalf("lost bootstrap failure evidence: %v", err)
	}
	instance, err := l.Ensure(context.Background(), spec)
	if err != nil || instance.DaemonID != "dedicated-daemon" {
		t.Fatal("repair did not finish readiness", err)
	}
	created, err := os.ReadFile(filepath.Join(dir, "created"))
	if err != nil || string(created) != "x" {
		t.Fatal("bootstrap retry allocated another VM", err)
	}
}

// Opt-in because this creates two real VMs and downloads a pinned guest image.
// The test never uses the developer's Docker daemon or application repositories.
func TestLocalVMIsolation(t *testing.T) {
	if os.Getenv("ENVCTL_VM_TEST") != "1" {
		t.Skip("set ENVCTL_VM_TEST=1 for real two-VM isolation test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	l := NewLima(t.TempDir())
	l.Log = os.Stderr
	suffix := time.Now().UTC().Format("20060102150405")
	a := DefaultSpec("envctl-test-a-" + suffix)
	b := DefaultSpec("envctl-test-b-" + suffix)
	a.MemoryGiB = 2
	b.MemoryGiB = 2
	for _, s := range []Spec{a, b} {
		t.Cleanup(func() {
			cleanup, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := l.Destroy(cleanup, s.ID); err != nil {
				t.Logf("cleanup %s: %v", s.ID, err)
			}
		})
	}
	ia, err := l.Ensure(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	ib, err := l.Ensure(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	if ia.DaemonID == ib.DaemonID {
		t.Fatal("shared Docker daemon")
	}
	if err = l.Exec(ctx, a.ID, Command{Args: []string{"sudo", "docker", "volume", "create", "envctl-isolation-marker"}}); err != nil {
		t.Fatal(err)
	}
	if err = l.Exec(ctx, b.ID, Command{Args: []string{"sudo", "docker", "volume", "inspect", "envctl-isolation-marker"}}); err == nil {
		t.Fatal("volume leaked across VMs")
	}
	if err = l.Exec(ctx, a.ID, Command{Args: []string{"sh", "-c", "test ! -e /Users && test ! -S /run/host-services/ssh-auth.sock"}}); err != nil {
		t.Fatal("host filesystem or agent exposed")
	}
	if err = l.Stop(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	again, err := l.Ensure(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	if again.DaemonID != ia.DaemonID {
		t.Fatal("restart changed daemon identity")
	}
	t.Logf("isolated Docker daemons: %s %s; Docker %s; Compose %s", ia.DaemonID, ib.DaemonID, ia.DockerVersion, ia.ComposeVersion)
}
