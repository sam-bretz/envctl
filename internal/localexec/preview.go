package localexec

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/sam-bretz/envctl/internal/dockerx"
	"github.com/sam-bretz/envctl/internal/engine"
	"github.com/sam-bretz/envctl/internal/gueststack"
	"github.com/sam-bretz/envctl/internal/preview"
	"github.com/sam-bretz/envctl/internal/workflow"
)

var _ engine.PreviewBackend = (*Backend)(nil)

// previewState holds the coordinator's forwards. They end with the process;
// the engine re-establishes them after a restart.
type previewState struct {
	once    sync.Once
	manager *preview.Manager
}

func (b *Backend) previewManager() *preview.Manager {
	b.previews.once.Do(func() {
		b.previews.manager = &preview.Manager{}
		if d, ok := b.Provider.(preview.Dialer); ok {
			b.previews.manager.Dialer = d
		}
	})
	return b.previews.manager
}

// previewReceipt keeps the host port stable across coordinator restarts.
type previewReceipt struct {
	Runtime  string `json:"runtime"`
	Service  string `json:"service"`
	Port     int    `json:"port"`
	HostPort int    `json:"host_port"`
}

// previewTarget reports whether this assignment's runtime hosts the preview:
// a configured parallel branch previews its child runtime; everything else
// previews the revision runtime.
func previewTarget(a engine.Assignment) bool {
	p := a.Revision.Config.Preview
	if p == nil {
		return false
	}
	kind := a.Revision.Config.Workflow.Nodes[p.Node].Kind
	branch := p.Node != "" && a.Revision.Config.Limits.Parallel > 1 && kind != "task" && kind != "plan"
	if a.Child != "" {
		return branch && a.Child == p.Node
	}
	return !branch
}

// guestPort finds the service's guest-loopback TCP binding for a container
// port. Only running containers count.
func guestPort(entries []dockerx.PSEntry, service string, target int) int {
	for _, e := range entries {
		if e.Service != service || e.State != "running" {
			continue
		}
		for _, p := range e.Publishers {
			if p.TargetPort == target && p.PublishedPort > 0 && p.Protocol != "udp" && (p.URL == "127.0.0.1" || p.URL == "::1") {
				return p.PublishedPort
			}
		}
	}
	return 0
}

// Preview keeps an explicit host-loopback forward to the configured guest
// service and returns its URL only while the service answers through it.
func (b *Backend) Preview(ctx context.Context, a engine.Assignment) (string, error) {
	id := a.Revision.Runtime.ID
	if !previewTarget(a) || !a.Revision.Runtime.Ready {
		return "", nil
	}
	cfg := *a.Revision.Config.Preview
	manager := b.previewManager()
	p, err := b.currentStack(a)
	if errors.Is(err, os.ErrNotExist) {
		manager.Close(id)
		return "", nil
	}
	if err != nil {
		return "", err
	}
	entries, err := b.stack(a).Status(ctx, p)
	if err != nil {
		return "", err
	}
	guest := guestPort(entries, cfg.Service, cfg.Port)
	if guest == 0 {
		// Not running, or the port is not published on guest loopback.
		manager.Close(id)
		return "", nil
	}
	dir, err := b.dir(a)
	if err != nil {
		return "", err
	}
	file := filepath.Join(dir, "preview.json")
	var receipt previewReceipt
	if raw, err := os.ReadFile(file); err == nil {
		_ = json.Unmarshal(raw, &receipt)
	}
	preferred := preview.PreferredPort(id + "/" + cfg.Service + "/" + strconv.Itoa(cfg.Port))
	if receipt.Runtime == id && receipt.Service == cfg.Service && receipt.Port == cfg.Port && receipt.HostPort > 0 {
		preferred = receipt.HostPort
	}
	host, err := manager.Ensure(id, guest, preferred)
	if err != nil {
		return "", err
	}
	if next := (previewReceipt{Runtime: id, Service: cfg.Service, Port: cfg.Port, HostPort: host}); next != receipt {
		if err = atomicJSON(file, next); err != nil {
			return "", err
		}
	}
	url := fmt.Sprintf("http://127.0.0.1:%d%s", host, cfg.RequestPath())
	if preview.Probe(ctx, url) != nil {
		return "", nil
	}
	return url, nil
}

func envName(service string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(service) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// endpoints lists each running service's guest-loopback TCP bindings, keyed
// by container port. Values are guest addresses: reachable from agents and
// checks inside the VM, not from the host.
func endpoints(entries []dockerx.PSEntry) map[string]map[int]int {
	out := map[string]map[int]int{}
	for _, e := range entries {
		if e.State != "running" {
			continue
		}
		for _, p := range e.Publishers {
			if p.PublishedPort > 0 && p.Protocol != "udp" && (p.URL == "127.0.0.1" || p.URL == "::1") {
				if out[e.Service] == nil {
					out[e.Service] = map[int]int{}
				}
				out[e.Service][p.TargetPort] = p.PublishedPort
			}
		}
	}
	return out
}

// primaryURL is the guest URL of a service's lowest published container port.
func primaryURL(ports map[int]int) string {
	targets := make([]int, 0, len(ports))
	for target := range ports {
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return ""
	}
	slices.Sort(targets)
	return fmt.Sprintf("http://127.0.0.1:%d", ports[targets[0]])
}

// endpointEnv derives ENVCTL_SERVICE_* variables for agents and checks.
func endpointEnv(entries []dockerx.PSEntry) map[string]string {
	env := map[string]string{}
	for service, ports := range endpoints(entries) {
		key := "ENVCTL_SERVICE_" + envName(service)
		env[key+"_HOST"] = "127.0.0.1"
		env[key+"_URL"] = primaryURL(ports)
		for target, published := range ports {
			env[fmt.Sprintf("%s_PORT_%d", key, target)] = strconv.Itoa(published)
		}
	}
	return env
}

// serviceEnv queries the prepared stack's live bindings. Stages without a
// stack (Task, Plan) receive no endpoints.
func (b *Backend) serviceEnv(ctx context.Context, a engine.Assignment, p *gueststack.Prepared) (map[string]string, error) {
	if p == nil {
		return map[string]string{}, nil
	}
	entries, err := b.stack(a).Status(ctx, *p)
	if err != nil {
		return nil, err
	}
	return endpointEnv(entries), nil
}

// withServiceURLs annotates runtime services with their guest endpoints.
func withServiceURLs(services []workflow.Service, entries []dockerx.PSEntry) []workflow.Service {
	urls := endpoints(entries)
	for i := range services {
		services[i].URL = primaryURL(urls[services[i].Name])
	}
	return services
}
