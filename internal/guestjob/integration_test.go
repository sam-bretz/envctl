package guestjob

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	vm "github.com/sam-bretz/envctl/internal/runtime"
)

func TestGuestJobReconnect(t *testing.T) {
	if os.Getenv("ENVCTL_VM_TEST") != "1" {
		t.Skip("set ENVCTL_VM_TEST=1 for real guest job lifetime test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	provider := vm.NewLima(t.TempDir())
	provider.Log = os.Stderr
	spec := vm.DefaultSpec("envctl-jobtest-" + time.Now().UTC().Format("20060102150405"))
	spec.MemoryGiB = 2
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		if err := provider.Destroy(ctx, spec.ID); err != nil {
			t.Log(err)
		}
	})
	if _, err := provider.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	client := Client{Provider: provider, Runtime: spec.ID}
	if err := client.Install(ctx); err != nil {
		t.Fatal(err)
	}
	if err := provider.Exec(ctx, spec.ID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "mkdir", "-p", "/work/envctl/reconnect"}}); err != nil {
		t.Fatal(err)
	}
	req := Request{ID: "attempt_reconnect", Args: []string{"sh", "-c", `printf x >> count; printf '%s\n' "$PROBE_SECRET"; sleep 3; printf 'finished\n'`}, Dir: "/work/envctl/reconnect", Env: map[string]string{"PROBE_SECRET": "synthetic-redaction-token"}, Secrets: []string{"synthetic-redaction-token"}, TimeoutSeconds: 30}
	if _, err := client.Submit(ctx, req); err != nil {
		t.Fatal(err)
	}
	// Construct a new client after the submitting SSH process has exited.
	client = Client{Provider: vm.NewLima(provider.Dir), Runtime: spec.ID}
	if _, err := client.Submit(ctx, req); err != nil {
		t.Fatal(err)
	}
	var status Status
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); {
		var err error
		status, err = client.Poll(ctx, req.ID, 0)
		if err != nil {
			t.Fatal(err)
		}
		if status.State == "completed" {
			break
		}
		if status.State != "running" && status.State != "starting" {
			t.Fatalf("unexpected job state: %+v", status)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if status.State != "completed" || status.ExitCode == nil || *status.ExitCode != 0 {
		t.Fatalf("job did not complete: %+v", status)
	}
	if status.Output != "[REDACTED]\nfinished\n" {
		t.Fatalf("wrong output: %+v", status)
	}
	var count strings.Builder
	if err := provider.Exec(ctx, spec.ID, vm.Command{Args: []string{"sudo", "-u", "envctl-agent", "cat", "/work/envctl/reconnect/count"}, Stdout: &count}); err != nil {
		t.Fatal(err)
	}
	if count.String() != "x" {
		t.Fatalf("duplicate execution: %q", count.String())
	}
	if err := provider.Stop(ctx, spec.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	status, err := client.Poll(ctx, req.ID, 0)
	if err != nil || status.State != "completed" {
		t.Fatalf("receipt lost after reboot: %+v, %v", status, err)
	}
	t.Log("systemd job survived submitter disconnect, executed once, redacted credentials, and retained its receipt across VM reboot")
}
