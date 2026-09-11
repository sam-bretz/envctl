package gueststack

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/dockerx"
	"github.com/sam-bretz/envctl/internal/manifest"
	vm "github.com/sam-bretz/envctl/internal/runtime"
)

func fixtureSpec() Spec {
	return Spec{ID: "attempt_one", Project: "revision_one", Root: "/work/envctl/repos/app", Stack: manifest.Stack{Files: []string{"compose.yaml"}}}
}
func TestTransformDecodesCanonicalDurationsWithoutHostFiles(t *testing.T) {
	s := fixtureSpec()
	raw := []byte(`{"name":"original","services":{"web":{"image":"busybox","environment":{"PASSWORD":"$$literal"},"ports":[{"target":8080,"published":"8080","host_ip":"0.0.0.0","protocol":"tcp"}],"healthcheck":{"test":["CMD","true"],"interval":"1s","timeout":"500ms"},"volumes":[{"type":"bind","source":"/work/envctl/repos/app","target":"/app"}]}}}`)
	out, services, err := transform(raw, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(services) != 1 || services[0] != "web" || !strings.Contains(string(out), "127.0.0.1") || !strings.Contains(string(out), "$$literal") || !strings.Contains(string(out), "500ms") {
		t.Fatalf("canonical model lost fields: %s", out)
	}
	for _, bad := range []string{
		`{"services":{"web":{"image":"busybox","env_file":[{"path":"/host/secret"}]}}}`,
		`{"services":{"web":{"image":"busybox","label_file":["/host/secret"]}}}`,
		`{"services":{"web":{"image":"busybox","volumes":[{"type":"bind","source":"/var/run/docker.sock","target":"/var/run/docker.sock"}]}}}`,
		`{"services":{"web":{"build":{"context":"/Users/developer/source"}}}}`,
	} {
		if _, _, err := transform([]byte(bad), s); err == nil {
			t.Fatalf("unsafe/unresolved model accepted: %s", bad)
		}
	}
}
func TestHealthRequiresEveryReplicaAndService(t *testing.T) {
	p := Prepared{Services: []string{"web", "db"}}
	good := []dockerx.PSEntry{{Service: "web", State: "running", Health: "healthy"}, {Service: "db", State: "running", Health: "healthy"}}
	if !Healthy(p, good) {
		t.Fatal("healthy services rejected")
	}
	if Healthy(p, nil) || Healthy(p, good[:1]) {
		t.Fatal("missing services treated as healthy")
	}
	good = append(good, dockerx.PSEntry{Service: "web", State: "running", Health: "starting"})
	if Healthy(p, good) {
		t.Fatal("starting replica treated as healthy")
	}
}

type failingExecutor struct{ calls int }

func (f *failingExecutor) Exec(_ context.Context, _ string, c vm.Command) error {
	f.calls++
	if c.Stdout != nil {
		_, _ = io.WriteString(c.Stdout, "private-value")
	}
	return errors.New("transport private-value")
}
func TestInputsStayInGuestAndErrorsDoNotLeak(t *testing.T) {
	f := &failingExecutor{}
	c := Client{Executor: f, Runtime: "vm"}
	s := fixtureSpec()
	s.Stack.Files = []string{"../../../../etc/passwd"}
	if _, err := c.Prepare(context.Background(), s); err == nil || f.calls != 0 {
		t.Fatal("unsafe input reached guest")
	}
	_, err := c.Prepare(context.Background(), fixtureSpec())
	if err == nil || strings.Contains(err.Error(), "private-value") {
		t.Fatal("raw diagnostic escaped", err)
	}
}
