package preview

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// guestDialer stands in for the SSH channel: it connects to "guest" ports
// served by in-process listeners and counts dials per runtime.
type guestDialer struct {
	mu    sync.Mutex
	ports map[int]string // guest port -> address of the service
	dials int
}

func (d *guestDialer) DialGuest(ctx context.Context, runtime string, port int) (io.ReadWriteCloser, error) {
	d.mu.Lock()
	addr, ok := d.ports[port]
	d.dials++
	d.mu.Unlock()
	if !ok {
		return nil, errors.New("connection refused")
	}
	var dialer net.Dialer
	return dialer.DialContext(ctx, "tcp", addr)
}

func TestForwardServesOnlyLoopbackAndIsReplacedOrClosed(t *testing.T) {
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "preview:"+r.URL.Path) }))
	defer web.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "other") }))
	defer other.Close()
	d := &guestDialer{ports: map[int]string{8080: strings.TrimPrefix(web.URL, "http://"), 9090: strings.TrimPrefix(other.URL, "http://")}}
	m := &Manager{Dialer: d}
	ctx := context.Background()

	port, err := m.Ensure("envctl-rev-a", 8080, PreferredPort("envctl-rev-a/web/8080"))
	if err != nil {
		t.Fatal(err)
	}
	url := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	if err = Probe(ctx, url); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(body) != "preview:/health" {
		t.Fatalf("forward returned %q", body)
	}
	if again, err := m.Ensure("envctl-rev-a", 8080, 1); err != nil || again != port {
		t.Fatal("reconciliation created a second forward", again, port, err)
	}
	if p, ok := m.Port("envctl-rev-a"); !ok || p != port {
		t.Fatal("active forward not reported")
	}
	// A different guest port replaces the forward rather than adding one.
	replaced, err := m.Ensure("envctl-rev-a", 9090, port)
	if err != nil {
		t.Fatal(err)
	}
	if err = Probe(ctx, fmt.Sprintf("http://127.0.0.1:%d/", replaced)); err != nil {
		t.Fatal(err)
	}
	m.Close("envctl-rev-a")
	if _, ok := m.Port("envctl-rev-a"); ok {
		t.Fatal("closed forward still reported")
	}
	if c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", replaced)); err == nil {
		c.Close()
		t.Fatal("closed forward still accepts connections")
	}
}

func TestProbeFailsWhenTheGuestServiceIsDown(t *testing.T) {
	m := &Manager{Dialer: &guestDialer{ports: map[int]string{}}}
	port, err := m.Ensure("envctl-rev-b", 8080, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close("envctl-rev-b")
	if Probe(context.Background(), fmt.Sprintf("http://127.0.0.1:%d/", port)) == nil {
		t.Fatal("probe passed without a reachable guest service")
	}
}

func TestForwardFallsBackWhenPreferredPortIsTakenAndRequiresADialer(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	taken := busy.Addr().(*net.TCPAddr).Port
	m := &Manager{Dialer: &guestDialer{ports: map[int]string{}}}
	port, err := m.Ensure("envctl-rev-c", 8080, taken)
	if err != nil || port == taken {
		t.Fatal("did not fall back from an occupied port", port, err)
	}
	m.Close("envctl-rev-c")
	if _, err = (&Manager{}).Ensure("envctl-rev-c", 8080, 0); err == nil {
		t.Fatal("forward without a provider dialer accepted")
	}
	var seen []string
	m = &Manager{Dialer: &guestDialer{}, Listen: func(network, address string) (net.Listener, error) {
		seen = append(seen, address)
		return net.Listen(network, address)
	}}
	if _, err = m.Ensure("envctl-rev-d", 8080, 0); err != nil {
		t.Fatal(err)
	}
	defer m.Close("envctl-rev-d")
	for _, address := range seen {
		if !strings.HasPrefix(address, "127.0.0.1:") {
			t.Fatal("preview bound beyond host loopback", address)
		}
	}
	if p := PreferredPort("x"); p < 41000 || p >= 49000 || p != PreferredPort("x") {
		t.Fatal("preferred port is not stable and bounded", p)
	}
}
