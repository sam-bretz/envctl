package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/guestjob"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/manifest"
	vm "github.com/sam-bretz/envctl/internal/runtime"
	"github.com/sam-bretz/envctl/internal/workflow"
)

// previewProvider answers Compose status and dials "guest" ports served by
// in-process listeners, standing in for the SSH stdio channel.
type previewProvider struct {
	vm.Provider
	mu        sync.Mutex
	webState  string
	addr      string
	dials     int
	destroyed []string
}

func (p *previewProvider) Exec(_ context.Context, _ string, c vm.Command) error {
	if !slices.Contains(c.Args, "ps") {
		return errors.New("unexpected guest command")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rows := []map[string]any{
		{"Name": "demo-web-1", "Service": "web", "State": p.webState, "Publishers": []map[string]any{{"URL": "127.0.0.1", "TargetPort": 8080, "PublishedPort": 18089, "Protocol": "tcp"}, {"URL": "127.0.0.1", "TargetPort": 9090, "PublishedPort": 19090, "Protocol": "tcp"}}},
		{"Name": "demo-db-1", "Service": "db", "State": "running", "Publishers": []map[string]any{{"URL": "", "TargetPort": 5432, "PublishedPort": 0, "Protocol": "tcp"}}},
		{"Name": "demo-dns-1", "Service": "dns", "State": "running", "Publishers": []map[string]any{{"URL": "127.0.0.1", "TargetPort": 53, "PublishedPort": 10053, "Protocol": "udp"}}},
	}
	for _, row := range rows {
		if err := json.NewEncoder(c.Stdout).Encode(row); err != nil {
			return err
		}
	}
	return nil
}
func (p *previewProvider) DialGuest(ctx context.Context, _ string, port int) (io.ReadWriteCloser, error) {
	p.mu.Lock()
	p.dials++
	addr := p.addr
	p.mu.Unlock()
	if port != 18089 || addr == "" {
		return nil, errors.New("connection refused")
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}
func (p *previewProvider) Destroy(_ context.Context, id string) error {
	p.destroyed = append(p.destroyed, id)
	return nil
}

func TestPreviewForwardLifecycleAndServiceEndpoints(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "web"+r.URL.Path) }))
	defer web.Close()
	b, a := fixture(t)
	p := &previewProvider{webState: "running", addr: strings.TrimPrefix(web.URL, "http://")}
	b.Provider = p
	a.Revision.Config.Preview = &workflow.Preview{Service: "web", Port: 8080, Path: "/health"}
	a.Revision.Runtime.Ready = true
	ctx := context.Background()

	if url, err := b.Preview(ctx, a); err != nil || url != "" {
		t.Fatal("preview advertised before a stack exists", url, err)
	}
	spec := gueststack.Spec{ID: "attempt_before", Project: "demo", Root: "/work/envctl/repos/app", Stack: manifest.Stack{Files: []string{"compose.yaml"}}}
	if err := b.saveStack(a, gueststack.Prepared{Spec: spec, File: "/var/lib/envctl/stacks/attempt_before/compose.yaml", Services: []string{"web", "db", "dns"}}); err != nil {
		t.Fatal(err)
	}
	url, err := b.Preview(ctx, a)
	if err != nil || !strings.HasPrefix(url, "http://127.0.0.1:") || !strings.HasSuffix(url, "/health") {
		t.Fatal("preview URL", url, err)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "web/health" {
		t.Fatalf("preview served %q", body)
	}
	if again, err := b.Preview(ctx, a); err != nil || again != url {
		t.Fatal("reconciliation changed or duplicated the forward", again, err)
	}

	// Endpoints: guest-loopback TCP bindings only, for agents and checks.
	env, err := b.serviceEnv(ctx, a, &gueststack.Prepared{Spec: spec, File: "/var/lib/envctl/stacks/attempt_before/compose.yaml"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ENVCTL_SERVICE_WEB_HOST": "127.0.0.1", "ENVCTL_SERVICE_WEB_URL": "http://127.0.0.1:18089", "ENVCTL_SERVICE_WEB_PORT_8080": "18089", "ENVCTL_SERVICE_WEB_PORT_9090": "19090"}
	if fmt.Sprint(env) != fmt.Sprint(want) {
		t.Fatalf("service environment %v, want %v", env, want)
	}
	if empty, err := b.serviceEnv(ctx, a, nil); err != nil || len(empty) != 0 {
		t.Fatal("stage without a stack received endpoints", empty, err)
	}
	status, err := b.RuntimeStatus(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range status.Services {
		if (s.Name == "web") != (s.URL == "http://127.0.0.1:18089") {
			t.Fatalf("service endpoint annotation %+v", s)
		}
	}

	// Coordinator restart: the forward dies with the process; a new backend
	// re-establishes it on the recorded host port.
	b.previewManager().Close(a.Revision.Runtime.ID)
	restarted := New(b.Store)
	restarted.Provider = p
	if again, err := restarted.Preview(ctx, a); err != nil || again != url {
		t.Fatal("restart did not re-establish the same preview", again, url, err)
	}
	// The guest service stops answering: no URL, but the forward is kept.
	p.mu.Lock()
	p.addr = ""
	p.mu.Unlock()
	if gone, err := restarted.Preview(ctx, a); err != nil || gone != "" {
		t.Fatal("unreachable preview advertised", gone, err)
	}
	if _, ok := restarted.previewManager().Port(a.Revision.Runtime.ID); !ok {
		t.Fatal("transient probe failure dropped the forward")
	}
	// The service is no longer running: the forward is removed.
	p.mu.Lock()
	p.webState = "exited"
	p.mu.Unlock()
	if gone, err := restarted.Preview(ctx, a); err != nil || gone != "" {
		t.Fatal("stopped service advertised", gone, err)
	}
	if _, ok := restarted.previewManager().Port(a.Revision.Runtime.ID); ok {
		t.Fatal("forward kept for a stopped service")
	}
	// Release tears the forward down with the runtime.
	p.mu.Lock()
	p.webState, p.addr = "running", strings.TrimPrefix(web.URL, "http://")
	p.mu.Unlock()
	if _, err = restarted.Preview(ctx, a); err != nil {
		t.Fatal(err)
	}
	if err = restarted.Release(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, ok := restarted.previewManager().Port(a.Revision.Runtime.ID); ok || !slices.Contains(p.destroyed, a.Revision.Runtime.ID) {
		t.Fatal("release left the preview forward open")
	}
	if c, err := net.Dial("tcp", strings.TrimSuffix(strings.TrimPrefix(url, "http://"), "/health")); err == nil {
		c.Close()
		t.Fatal("released preview still accepts connections")
	}
}

// endpointProvider answers Compose status like previewProvider and records
// guest job submissions like submitFixture.
type endpointProvider struct {
	previewProvider
	jobs *submitFixture
}

func (p *endpointProvider) Exec(ctx context.Context, id string, c vm.Command) error {
	if slices.Contains(c.Args, "ps") {
		return p.previewProvider.Exec(ctx, id, c)
	}
	return p.jobs.Exec(ctx, id, c)
}

func TestChecksReceiveGuestServiceEndpoints(t *testing.T) {
	b, a := fixture(t)
	p := &endpointProvider{previewProvider: previewProvider{webState: "running"}, jobs: &submitFixture{submitted: map[string]guestjob.Request{}}}
	b.Provider = p
	code := a.Revision.Config.Workflow.Nodes["code"]
	code.Checks = []workflow.Check{{Name: "unit", Command: []string{"make", "test"}}}
	a.Revision.Config.Workflow.Nodes["code"] = code
	stack := gueststack.Prepared{Spec: gueststack.Spec{ID: "attempt_after", Project: "demo", Root: "/work/envctl/repos/app", Stack: manifest.Stack{Files: []string{"compose.yaml"}}}, File: "/var/lib/envctl/stacks/attempt_after/compose.yaml"}
	r := &attemptRecord{Binding: attemptBinding(a), Phase: "checks", Stack: &stack, Directories: map[string]string{"app": "/work/envctl/repos/app"}, Cursors: map[string]int64{}, Logs: map[string]string{}}
	if err := b.save(a, r); err != nil {
		t.Fatal(err)
	}
	if o, err := b.Poll(context.Background(), a); err != nil || o.State != "running" {
		t.Fatal("check was not dispatched", o, err)
	}
	req := p.jobs.submitted[a.Attempt.ID+"_check_0"]
	if req.Env["ENVCTL_SERVICE_WEB_URL"] != "http://127.0.0.1:18089" || req.Env["ENVCTL_SERVICE_WEB_PORT_8080"] != "18089" || req.Env["PYTHONDONTWRITEBYTECODE"] != "1" {
		t.Fatalf("check environment %v", req.Env)
	}
	for k := range req.Env {
		if strings.Contains(k, "DB") || strings.Contains(k, "DNS") {
			t.Fatal("unpublished or UDP service leaked into endpoints", k)
		}
	}
}

func TestPreviewTargetsTheConfiguredRuntime(t *testing.T) {
	_, a := fixture(t)
	a.Revision.Config.Preview = &workflow.Preview{Service: "web", Port: 8080}
	if !previewTarget(a) {
		t.Fatal("revision runtime not previewed by default")
	}
	a.Revision.Config.Preview.Node = "code"
	if !previewTarget(a) {
		t.Fatal("serial node preview must use the revision runtime")
	}
	a.Revision.Config.Limits.Parallel = 2
	if previewTarget(a) {
		t.Fatal("parallel branch preview claimed by the revision runtime")
	}
	a.Revision.ChildRuntimes = map[string]*workflow.ChildRuntime{"code": {Runtime: a.Revision.Runtime}, "design": {Runtime: a.Revision.Runtime}}
	child := a
	child.Child = "code"
	if !previewTarget(child) {
		t.Fatal("configured branch runtime not previewed")
	}
	child.Child = "design"
	if previewTarget(child) {
		t.Fatal("sibling branch runtime previewed")
	}
	a.Revision.Config.Preview = nil
	if previewTarget(a) {
		t.Fatal("preview without configuration")
	}
	var _ engine.PreviewBackend = (*Backend)(nil)
}
