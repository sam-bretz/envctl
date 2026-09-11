package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	osexec "os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/runstore"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// Creates and destroys its own dedicated VM. It never adopts another VM.
// Set ENVCTL_PREVIEW_KEEP=1 to retain the VM after a failure for diagnosis.
func TestRealPreviewForwardThroughDedicatedVM(t *testing.T) {
	if os.Getenv("ENVCTL_PREVIEW_TEST") != "1" {
		t.Skip("set ENVCTL_PREVIEW_TEST=1 to create a dedicated preview acceptance VM")
	}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Minute)
	defer cancel()
	state, err := os.MkdirTemp("", "envctl-preview-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Log("retained preview acceptance state:", state)
	logFile, err := os.Create(filepath.Join(state, "lima.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()
	id := "envctl-preview-" + time.Now().UTC().Format("20060102150405")
	provider := vm.NewLima(state)
	provider.Log = logFile
	spec := vm.DefaultSpec(id)
	spec.MemoryGiB = 2
	destroyed := false
	defer func() {
		if destroyed || (t.Failed() && os.Getenv("ENVCTL_PREVIEW_KEEP") == "1") {
			return
		}
		clean, done := context.WithTimeout(context.Background(), 5*time.Minute)
		defer done()
		if err := provider.Destroy(clean, id); err != nil {
			t.Errorf("destroy preview VM %s: %v", id, err)
		}
	}()
	proof := map[string]any{"runtime": id}
	save := func() {
		raw, _ := json.MarshalIndent(proof, "", "  ")
		if err := os.WriteFile(filepath.Join(state, "preview-proof.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = provider.Ensure(ctx, spec); err != nil {
		t.Fatal(err)
	}
	proof["provisioned_seconds"] = time.Since(started).Seconds()
	save()
	if err = (guestjob.Client{Provider: provider, Runtime: id}).Install(ctx); err != nil {
		t.Fatal(err)
	}
	root := "/work/envctl/fixtures/preview"
	// A per-run marker distinguishes this guest service from anything else
	// that may already listen on the same port number on the host.
	marker := "preview-ready-" + id
	exec := func(args []string, input string) {
		t.Helper()
		if err := provider.Exec(ctx, id, vm.Command{Args: args, Stdin: strings.NewReader(input), Stdout: io.Discard}); err != nil {
			t.Fatal(err)
		}
	}
	exec([]string{"sudo", "install", "-d", "-o", "envctl-agent", "-g", "envctl-agent", root}, "")
	exec([]string{"sudo", "tee", root + "/compose.yaml"}, `services:
  web:
    image: busybox:1.37.0
    command: [sh, -c, 'mkdir -p /www; echo `+marker+` > /www/index.html; httpd -f -p 8080 -h /www']
    ports: ['8080']
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']
      interval: 1s
      timeout: 1s
      retries: 30
`)

	// Production stack path: guest normalization, loopback-only publication.
	stacks := gueststack.Client{Executor: provider, Runtime: id}
	prepared, err := stacks.Prepare(ctx, gueststack.Spec{ID: "preview_stack", Project: "preview", Root: root, Stack: manifest.Stack{Files: []string{"compose.yaml"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = stacks.Up(ctx, prepared, 180); err != nil {
		t.Fatal(err)
	}
	entries, err := stacks.Status(ctx, prepared)
	if err != nil {
		t.Fatal(err)
	}
	guest := guestPort(entries, "web", 8080)
	if guest == 0 {
		t.Fatalf("web has no guest-loopback binding: %#v", entries)
	}
	proof["guest_port"] = guest

	store, err := runstore.Open(filepath.Join(state, "store"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c, err := workflow.Parse([]byte("version: 2\nproject: preview\nrepositories: [{id: app, url: /source}]\nworkflow: {template: feature}\npreview: {service: web, port: 8080}\n"))
	if err != nil {
		t.Fatal(err)
	}
	run, err := workflow.NewRun("preview acceptance", "serve a preview", "acceptance-test", c, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	// A completed revision keeps its runtime for inspection; its preview is
	// reconciled by the engine like any other ready runtime.
	run.Current().State = "completed"
	run.Current().Runtime = workflow.RuntimeState{ID: id, Provider: "lima", Location: "local", Ready: true, State: "running"}
	if _, err = store.Create(ctx, "create", "preview acceptance", run); err != nil {
		t.Fatal(err)
	}
	backend := New(store)
	backend.Provider = provider
	assignment := engine.Assignment{Run: *run, Revision: *run.Current()}
	if err = backend.saveStack(assignment, prepared); err != nil {
		t.Fatal(err)
	}
	advertised := func(b *Backend) string {
		t.Helper()
		e := &engine.Engine{Store: store, Backend: b}
		deadline := time.Now().Add(2 * time.Minute)
		for time.Now().Before(deadline) {
			if err := e.Tick(ctx); err != nil {
				t.Fatal(err)
			}
			current, err := store.Get(ctx, run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if url := current.Current().Runtime.PreviewURL; url != "" {
				return url
			}
			time.Sleep(11 * time.Second) // engine preview interval
		}
		t.Fatal("engine never advertised a reachable preview")
		return ""
	}
	fetch := func(url string) string {
		t.Helper()
		resp, err := (&http.Client{Timeout: 20 * time.Second}).Get(url)
		if err != nil {
			t.Fatal("host could not fetch the preview", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return strings.TrimSpace(string(body))
	}
	url := advertised(backend)
	if !strings.HasPrefix(url, "http://127.0.0.1:") || fetch(url) != marker {
		t.Fatal("preview did not serve the guest service", url)
	}
	proof["preview_url"] = url
	save()

	// Isolation: the guest port itself must not appear on the host. Lima's
	// forwarding detection is asynchronous, so allow it time first.
	// Another host process may legitimately own the same port number (host
	// Docker uses the same ephemeral range), so require this run's marker.
	time.Sleep(15 * time.Second)
	if resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(fmt.Sprintf("http://127.0.0.1:%d/", guest)); err == nil {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if strings.Contains(string(body), marker) {
			t.Fatalf("guest port %d serves this VM's service on the host: automatic forwarding is active", guest)
		}
		proof["guest_port_number_used_by_unrelated_host_process"] = true
	}
	if out, err := osexec.Command("lsof", "-nP", fmt.Sprintf("-iTCP:%d", guest), "-sTCP:LISTEN", "-Fc").Output(); err == nil && strings.Contains(string(out), "climactl") {
		t.Fatalf("Lima holds a host listener for guest port %d", guest)
	}
	proof["guest_service_not_forwarded_to_host"] = true

	// Coordinator restart: the forward dies with the process and a new
	// backend re-establishes it on the recorded port, without duplicates.
	backend.previewManager().Close(id)
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(strings.TrimSuffix(url, "/"), "http://"), 2*time.Second); err == nil {
		conn.Close()
		t.Fatal("simulated coordinator exit left the forward open")
	}
	if _, err = store.Mutate(ctx, run.ID, mustVersion(t, store, run.ID), "clear-preview", "test.clear", nil, func(r *workflow.Run) error { r.Current().Runtime.PreviewURL = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	restarted := New(store)
	restarted.Provider = provider
	if again := advertised(restarted); again != url || fetch(again) != marker {
		t.Fatal("restart did not re-establish the same preview", again, url)
	}
	if again, err := restarted.Preview(ctx, assignment); err != nil || again != url {
		t.Fatal("repeated reconciliation changed the forward", again, err)
	}
	proof["restart_reconciled"] = true
	save()

	// Release closes the forward and destroys the dedicated VM.
	if err = restarted.Release(ctx, assignment); err != nil {
		t.Fatal(err)
	}
	if conn, err := net.DialTimeout("tcp", strings.TrimPrefix(strings.TrimSuffix(url, "/"), "http://"), 2*time.Second); err == nil {
		conn.Close()
		t.Fatal("released preview still accepts connections")
	}
	if _, err = provider.Inspect(ctx, id); !errors.Is(err, vm.ErrMissing) {
		t.Fatal("release did not destroy the preview VM", err)
	}
	destroyed = true
	proof["released"] = true
	proof["seconds"] = time.Since(started).Seconds()
	save()
	t.Logf("preview served from host loopback, guest port not auto-forwarded, restart reconciled, release closed forward and destroyed VM in %.1fs", time.Since(started).Seconds())
}

func mustVersion(t *testing.T, store *runstore.Store, id string) int64 {
	t.Helper()
	run, err := store.Get(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run.Version
}
