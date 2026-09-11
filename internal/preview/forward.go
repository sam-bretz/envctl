// Package preview owns explicit host-loopback forwards to one guest service.
// A forward lives only as long as the coordinator process that holds it; the
// coordinator re-establishes it on restart. Nothing binds beyond 127.0.0.1.
package preview

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// Dialer opens one stream to a TCP port on a guest's loopback.
type Dialer interface {
	DialGuest(ctx context.Context, runtime string, port int) (io.ReadWriteCloser, error)
}

type Manager struct {
	Dialer Dialer
	// Listen is injectable for tests; production binds with net.Listen.
	Listen func(network, address string) (net.Listener, error)
	mu     sync.Mutex
	active map[string]*forward
}

type forward struct {
	guest    int
	listener net.Listener
	mu       sync.Mutex
	conns    map[io.Closer]struct{}
	closed   bool
}

// PreferredPort derives a stable host port for a preview key so a restarted
// coordinator normally offers the same URL.
func PreferredPort(key string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return 41000 + int(h.Sum32()%8000)
}

// Ensure keeps exactly one forward per runtime and returns its host port. A
// live forward to the same guest port is reused; one to another port is
// replaced. The preferred port falls back to an OS-assigned loopback port.
func (m *Manager) Ensure(runtime string, guest, preferred int) (int, error) {
	if m.Dialer == nil {
		return 0, errors.New("runtime provider does not support preview forwarding")
	}
	if guest < 1 || guest > 65535 {
		return 0, errors.New("invalid guest preview port")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active == nil {
		m.active = map[string]*forward{}
	}
	if f := m.active[runtime]; f != nil {
		if f.guest == guest {
			return f.listener.Addr().(*net.TCPAddr).Port, nil
		}
		f.close()
		delete(m.active, runtime)
	}
	listen := m.Listen
	if listen == nil {
		listen = net.Listen
	}
	l, err := listen("tcp", fmt.Sprintf("127.0.0.1:%d", preferred))
	if err != nil {
		if l, err = listen("tcp", "127.0.0.1:0"); err != nil {
			return 0, errors.New("no host loopback port is available for the preview")
		}
	}
	addr, ok := l.Addr().(*net.TCPAddr)
	if !ok || !addr.IP.IsLoopback() {
		l.Close()
		return 0, errors.New("preview listener is not bound to host loopback")
	}
	f := &forward{guest: guest, listener: l, conns: map[io.Closer]struct{}{}}
	m.active[runtime] = f
	go m.serve(runtime, f)
	return addr.Port, nil
}

// Port reports the host port of a runtime's live forward.
func (m *Manager) Port(runtime string) (int, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f := m.active[runtime]; f != nil {
		return f.listener.Addr().(*net.TCPAddr).Port, true
	}
	return 0, false
}

// Close tears down a runtime's forward and its open connections.
func (m *Manager) Close(runtime string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if f := m.active[runtime]; f != nil {
		f.close()
		delete(m.active, runtime)
	}
}

func (f *forward) track(c io.Closer) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return false
	}
	f.conns[c] = struct{}{}
	return true
}
func (f *forward) untrack(c io.Closer) {
	f.mu.Lock()
	delete(f.conns, c)
	f.mu.Unlock()
}
func (f *forward) close() {
	f.mu.Lock()
	f.closed = true
	conns := f.conns
	f.conns = map[io.Closer]struct{}{}
	f.mu.Unlock()
	_ = f.listener.Close()
	for c := range conns {
		_ = c.Close()
	}
}

func (m *Manager) serve(runtime string, f *forward) {
	for {
		conn, err := f.listener.Accept()
		if err != nil {
			return
		}
		go m.relay(runtime, f, conn)
	}
}

func (m *Manager) relay(runtime string, f *forward, conn net.Conn) {
	if !f.track(conn) {
		conn.Close()
		return
	}
	defer f.untrack(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	stream, err := m.Dialer.DialGuest(ctx, runtime, f.guest)
	cancel()
	if err != nil {
		conn.Close()
		return
	}
	if !f.track(stream) {
		conn.Close()
		stream.Close()
		return
	}
	defer f.untrack(stream)
	var once sync.Once
	stop := func() { once.Do(func() { conn.Close(); stream.Close() }) }
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(stream, conn); stop(); done <- struct{}{} }()
	go func() { _, _ = io.Copy(conn, stream); stop(); done <- struct{}{} }()
	<-done
	<-done
}

// Probe verifies the service answers HTTP through the forward. Any response
// status counts: the preview is reachable even if the path returns an error.
func Probe(ctx context.Context, url string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	// A fresh connection per probe: a pooled one may already be tunneled to a
	// service that has since stopped, which would hide the outage.
	client := &http.Client{
		Transport:     &http.Transport{DisableKeepAlives: true, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		return errors.New("preview did not answer through its forward")
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.Body.Close()
}
