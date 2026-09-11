package gueststack

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/manifest"
	vm "github.com/sam-bretz/envctl/internal/runtime"
)

// Run against an explicitly selected, owned acceptance VM. This test creates
// and destroys only its own stack and volume; it never adopts arbitrary VMs.
func TestRealGuestComposeLifecycle(t *testing.T) {
	if os.Getenv("ENVCTL_STACK_TEST") != "1" {
		t.Skip("set ENVCTL_STACK_TEST=1 with owned VM/state directory")
	}
	state, name := os.Getenv("ENVCTL_AGENT_STATE_DIR"), os.Getenv("ENVCTL_AGENT_VM")
	if state == "" || name == "" {
		t.Fatal("explicit owned VM and state directory required")
	}
	provider := vm.NewLima(state)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if _, err := provider.Inspect(ctx, name); err != nil {
		t.Fatal(err)
	}
	if err := (guestjob.Client{Provider: provider, Runtime: name}).Install(ctx); err != nil {
		t.Fatal(err)
	}
	id := "stack_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	root := "/work/envctl/fixtures/" + id
	command := func(args []string, input string) string {
		t.Helper()
		var out bytes.Buffer
		if err := provider.Exec(ctx, name, vm.Command{Args: args, Stdin: strings.NewReader(input), Stdout: &out}); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	command([]string{"sudo", "install", "-d", "-o", "envctl-agent", "-g", "envctl-agent", root}, "")
	compose := `services:
  web:
    image: busybox:1.37.0
    command: [sh, -c, 'mkdir -p /www; echo ready > /www/index.html; httpd -f -p 8080 -h /www']
    environment:
      LITERAL: '$$literal'
    ports: ['8080']
    volumes: [data:/data]
    healthcheck:
      test: [CMD, wget, -q, -O, /dev/null, 'http://127.0.0.1:8080/']
      interval: 1s
      timeout: 1s
      retries: 20
volumes:
  data: {}
`
	command([]string{"sudo", "tee", root + "/compose.yaml"}, compose)
	c := Client{Executor: provider, Runtime: name}
	s := Spec{ID: id, Project: id, Root: root, Stack: manifest.Stack{Files: []string{"compose.yaml"}}, Expose: []manifest.Expose{{Service: "web", Port: 8080}}}
	p, err := c.Prepare(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		clean, done := context.WithTimeout(context.Background(), time.Minute)
		defer done()
		if err := c.Down(clean, p, true); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})
	if err = c.Up(ctx, p, 90); err != nil {
		t.Fatal(err)
	}
	entries, err := c.Status(ctx, p)
	if err != nil || !Healthy(p, entries) {
		t.Fatalf("status: %#v %v", entries, err)
	}
	port := 0
	for _, e := range entries {
		for _, binding := range e.Publishers {
			if binding.TargetPort == 8080 {
				if binding.URL != "127.0.0.1" {
					t.Fatalf("non-loopback binding: %#v", binding)
				}
				port = binding.PublishedPort
			}
		}
	}
	if port == 0 {
		t.Fatal("missing guest endpoint")
	}
	if got := command([]string{"curl", "--fail", "--silent", fmt.Sprintf("http://127.0.0.1:%d/", port)}, ""); strings.TrimSpace(got) != "ready" {
		t.Fatal("guest endpoint failed")
	}
	value := command(p.command("exec", "-T", "web", "printenv", "LITERAL"), "")
	if strings.TrimSpace(value) != "$literal" {
		t.Fatalf("environment was interpolated twice: %q", value)
	}
	command(p.command("exec", "-T", "web", "sh", "-c", "echo preserved > /data/marker"), "")
	// A reconnect must use its frozen stack even if a worker edits the source.
	command([]string{"sudo", "tee", root + "/compose.yaml"}, "not valid: [")
	again, err := c.Prepare(ctx, s)
	if err != nil || again.Digest != p.Digest {
		t.Fatal("receipt replay failed", err)
	}
	if err = c.Down(ctx, p, false); err != nil {
		t.Fatal(err)
	}
	if err = c.Up(ctx, p, 90); err != nil {
		t.Fatal(err)
	}
	if got := command(p.command("exec", "-T", "web", "cat", "/data/marker"), ""); strings.TrimSpace(got) != "preserved" {
		t.Fatal("volume was lost on restart")
	}
	// Health loss must be observed, not inferred from a previous successful up.
	command(p.command("stop", "web"), "")
	entries, err = c.Status(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	if Healthy(p, entries) {
		t.Fatal("stopped service still ready")
	}
	t.Log("guest normalization, literal environment, loopback endpoint, immutable replay, volume restart and health loss verified")
}
