package compose

import (
	"context"
	"github.com/compose-spec/compose-go/v2/types"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sam-bretz/envctl/internal/manifest"
	"github.com/sam-bretz/envctl/internal/ports"
)

func TestTransformPreservesGuestPathsWithoutHostAccess(t *testing.T) {
	guestPath := "/work/envctl/revisions/rev_test/attempts/attempt_code/app"
	project := &types.Project{Services: types.Services{"app": types.ServiceConfig{
		Name: "app", ContainerName: "fixed", Build: &types.BuildConfig{Context: guestPath, Dockerfile: "Dockerfile"},
		Volumes: []types.ServiceVolumeConfig{{Type: "bind", Source: guestPath, Target: "/app"}},
		Ports:   []types.ServicePortConfig{{Target: 8000, Published: "8000"}},
	}}}
	raw, result, err := Transform(project, Options{Project: "envctl-revision", Env: "rev_test", Backend: "lima", Mode: ModeDomains})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), guestPath) || project.Services["app"].ContainerName != "envctl-revision-fixed" || project.Services["app"].Labels[LabelBackend] != "lima" {
		t.Fatal("guest project was not transformed correctly")
	}
	if len(project.Services["app"].Ports) != 0 || len(result.Services) != 1 {
		t.Fatal("shared render transformations were skipped")
	}
	if result.File != "" || result.EnvFile != "" {
		t.Fatal("pure transformation unexpectedly wrote files")
	}
}

const sample = `services:
  postgres:
    image: postgres:17
    container_name: pg
    ports:
      - "5433:5432"
    volumes:
      - pgdata:/var/lib/postgresql/data
  api:
    image: example/api:${TAG:-latest}
    ports:
      - "8787:8787"
      - "9000"
    depends_on: [postgres]
volumes:
  pgdata:
`

func setup(t *testing.T) *manifest.Manifest {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}
	mf := "version: 1\nproject: mg\nstack:\n  files: [docker-compose.yml]\n"
	if err := os.WriteFile(filepath.Join(dir, manifest.FileName), []byte(mf), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := manifest.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRenderDomainsStripsHostPorts(t *testing.T) {
	m := setup(t)
	res, err := Render(context.Background(), Options{
		Manifest: m, Env: "feat-a", Branch: "feat/a", Project: "mg-feat-a", Backend: "local",
		Mode: ModeDomains, OutDir: filepath.Join(m.Dir, ".envctl", "feat-a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(res.File)
	out := string(raw)
	for _, bad := range []string{"5433", "8787:8787", "published: \"8787\""} {
		if strings.Contains(out, bad) {
			t.Errorf("rendered file still binds host port %q:\n%s", bad, out)
		}
	}
	if !strings.Contains(out, "name: mg-feat-a") {
		t.Errorf("project name missing:\n%s", out)
	}
	if !strings.Contains(out, "container_name: mg-feat-a-pg") {
		t.Errorf("container_name not namespaced:\n%s", out)
	}
	if !strings.Contains(out, LabelEnv+": feat-a") || !strings.Contains(out, LabelFeature+": feat-a") {
		t.Errorf("env labels missing:\n%s", out)
	}
	if !strings.Contains(strings.Join(res.Env, "\n"), "ENVCTL_BRANCH=feat/a") {
		t.Errorf("env missing branch line")
	}
	if len(res.Published) != 0 {
		t.Errorf("domains mode published ports: %+v", res.Published)
	}
	joined := strings.Join(res.Env, "\n")
	if !strings.Contains(joined, "ENVCTL_HOST_POSTGRES=postgres.mg-feat-a.orb.local") {
		t.Errorf("env missing domain host:\n%s", joined)
	}
}

func TestRenderRegistryAllocatesStablePorts(t *testing.T) {
	m := setup(t)
	reg, err := ports.Open(filepath.Join(t.TempDir(), "ports.json"), 47100, 47120)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{
		Manifest: m, Env: "feat-b", Project: "mg-feat-b", Backend: "local",
		Mode: ModeRegistry, Registry: reg, OutDir: filepath.Join(m.Dir, ".envctl", "feat-b"),
	}
	res, err := Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Published) != 2 {
		t.Fatalf("want 2 published ports, got %+v", res.Published)
	}
	first := map[string]int{}
	for _, p := range res.Published {
		if p.Host < 47100 || p.Host > 47120 {
			t.Errorf("port %d outside range", p.Host)
		}
		first[p.Service] = p.Host
	}
	raw, _ := os.ReadFile(res.File)
	if strings.Contains(string(raw), "5433:5432") || strings.Contains(string(raw), "published: \"5433\"") {
		t.Errorf("original host port survived:\n%s", raw)
	}
	if !strings.Contains(string(raw), "host_ip: 127.0.0.1") {
		t.Errorf("registry ports should bind loopback only:\n%s", raw)
	}
	res2, err := Render(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res2.Published {
		if first[p.Service] != p.Host {
			t.Errorf("port for %s changed between renders: %d -> %d", p.Service, first[p.Service], p.Host)
		}
	}
	joined := strings.Join(res.Env, "\n")
	if !strings.Contains(joined, "ENVCTL_PORT_POSTGRES_5432=") {
		t.Errorf("env missing port variable:\n%s", joined)
	}
}
