package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// memNetwork gives forwarding tests real byte streams without kernel sockets.
type memNetwork struct {
	mu        sync.Mutex
	listeners map[string]*memListener
	next      int
}
type memListener struct {
	network     *memNetwork
	address     *net.TCPAddr
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func newMemNetwork() *memNetwork {
	return &memNetwork{listeners: map[string]*memListener{}, next: 40000}
}
func (n *memNetwork) listen(_, address string) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	a, e := net.ResolveTCPAddr("tcp", address)
	if e != nil {
		return nil, e
	}
	if a.Port == 0 {
		n.next++
		a.Port = n.next
	}
	if n.listeners[a.String()] != nil {
		return nil, errors.New("address already in use")
	}
	l := &memListener{network: n, address: a, connections: make(chan net.Conn, 16), closed: make(chan struct{})}
	n.listeners[a.String()] = l
	return l, nil
}
func (n *memNetwork) dial(ctx context.Context, address string) (net.Conn, error) {
	n.mu.Lock()
	l := n.listeners[address]
	n.mu.Unlock()
	if l == nil {
		return nil, errors.New("connection refused")
	}
	a, b := net.Pipe()
	select {
	case <-l.closed:
		a.Close()
		b.Close()
		return nil, net.ErrClosed
	case <-ctx.Done():
		a.Close()
		b.Close()
		return nil, ctx.Err()
	case l.connections <- b:
		return a, nil
	}
}
func (l *memListener) Accept() (net.Conn, error) {
	select {
	case <-l.closed:
		return nil, net.ErrClosed
	case c := <-l.connections:
		return c, nil
	}
}
func (l *memListener) Close() error {
	l.once.Do(func() {
		l.network.mu.Lock()
		delete(l.network.listeners, l.address.String())
		close(l.closed)
		l.network.mu.Unlock()
	})
	return nil
}
func (l *memListener) Addr() net.Addr { return l.address }

type testPeer struct {
	mu       sync.Mutex
	next     int
	bound    map[string]int
	requests []Control
	request  func(Control) error
	reverse  func(context.Context, string, int) (net.Conn, error)
}

func newTestPeer() *testPeer                 { return &testPeer{next: 22000, bound: map[string]int{}} }
func (p *testPeer) Context() context.Context { return context.Background() }
func (p *testPeer) Request(ctx context.Context, m Control) (Control, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, m)
	if p.request != nil {
		if err := p.request(m); err != nil {
			return Control{}, err
		}
	}
	switch m.Op {
	case "bind":
		if m.Port == 0 {
			m.Port = p.next
			p.next++
		}
		p.bound[m.Rule] = m.Port
	case "remove", "forget":
		delete(p.bound, m.Rule)
	}
	return m, nil
}
func (p *testPeer) Reverse(ctx context.Context, id string, port int) (net.Conn, error) {
	if p.reverse != nil {
		return p.reverse(ctx, id, port)
	}
	return nil, errors.New("no callback")
}
func forwardingFixture(t *testing.T) (*Gateway, *fixtureBackend, *memNetwork) {
	t.Helper()
	b := fakeBackend(t)
	g := NewGateway(engineFixture(t), Backend{Dial: b.dial})
	n := newMemNetwork()
	g.Forwards.Listen = n.listen
	g.Forwards.Attach("a", newTestPeer())
	g.Forwards.Attach("b", newTestPeer())
	t.Cleanup(g.Forwards.Close)
	return g, b, n
}
func scopeA() Scope { return Scope{Group: "a", Identity: "spiffe://test/alice", Tunnel: "a"} }
func scopeB() Scope { return Scope{Group: "b", Identity: "spiffe://test/bob", Tunnel: "b"} }
func waitFor(t *testing.T, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !f() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(time.Millisecond)
	}
}
func echoCheck(t *testing.T, c net.Conn, value string) {
	t.Helper()
	c.SetDeadline(time.Now().Add(time.Second))
	if _, err := io.WriteString(c, value); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, len(value))
	if _, err := io.ReadFull(c, b); err != nil || string(b) != value {
		t.Fatalf("echo %q: %v", b, err)
	}
}
func TestForwardClientBindingIsolationAndLocks(t *testing.T) {
	g, _, _ := forwardingFixture(t)
	s := scopeA()
	d := selectDevice(t, g.Engine, s, "A")
	var out bytes.Buffer
	g.Forwards.Forward(context.Background(), &out, s, d, "tcp:0;tcp:80")
	if out.String() != "OKAYOKAY000522000" {
		t.Fatal(out.String())
	}
	r := g.Forwards.find(g.Forwards.key(s, "tcp:22000"))
	if r == nil {
		t.Fatal("missing rule")
	}
	client, server := net.Pipe()
	defer client.Close()
	go func() { defer server.Close(); g.Forwards.OpenForward(context.Background(), s, r.id, server) }()
	echoCheck(t, client, "shared")
	lease, err := g.Engine.CreateLease("spiffe://test/alice", "a", "A", time.Second, time.Minute)
	if err != nil || lease.State != "queued" {
		t.Fatalf("stream did not hold read lock: %+v %v", lease, err)
	}
	client.Close()
	waitFor(t, func() bool { l, _ := g.Engine.Lease(lease.ID); return l.State == "active" })
	other := scopeA()
	other.Tunnel = "other"
	g.Forwards.Attach(other.Tunnel, newTestPeer())
	out.Reset()
	g.Forwards.List(&out, other)
	if strings.Contains(out.String(), "22000") {
		t.Fatal("cross-session forward leaked")
	}
	g.Engine.ReleaseLease(lease.ID, "spiffe://test/alice")
	out.Reset()
	g.Forwards.Forward(context.Background(), &out, s, d, "norebind:tcp:22000;tcp:80")
	if !strings.HasPrefix(out.String(), "FAIL") {
		t.Fatal("norebind ignored")
	}
	out.Reset()
	g.Forwards.Forward(context.Background(), &out, s, selectDevice(t, g.Engine, s, "B"), "tcp:22000;tcp:80")
	if !strings.HasPrefix(out.String(), "FAIL") {
		t.Fatal("cross-device rebind accepted")
	}
}
func TestReverseClientCallbackAndStaleCleanup(t *testing.T) {
	g, b, n := forwardingFixture(t)
	s := scopeA()
	d := selectDevice(t, g.Engine, s, "A")
	peer := newTestPeer()
	peer.reverse = func(ctx context.Context, rule string, port int) (net.Conn, error) {
		if port != 18080 {
			t.Error("wrong client target")
		}
		a, b := net.Pipe()
		go func() { defer b.Close(); io.Copy(b, b) }()
		return a, nil
	}
	g.Forwards.Attach(s.Tunnel, peer)
	var out bytes.Buffer
	g.Forwards.Reverse(context.Background(), &out, s, d, "forward:tcp:8080;tcp:18080")
	if out.String() != "OKAYOKAY" {
		t.Fatal(out.String())
	}
	old := g.Forwards.find(g.Forwards.reverseKey(d, "tcp:8080"))
	b.mu.Lock()
	target := b.reverse["tcp:8080"]
	b.mu.Unlock()
	port, _ := tcpPort(target, false)
	data, err := n.dial(context.Background(), net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	echoCheck(t, data, "reverse")
	data.Close()
	out.Reset()
	g.Forwards.Reverse(context.Background(), &out, scopeB(), d, "forward:tcp:8080;tcp:18081")
	if out.String() != "OKAYOKAY" {
		t.Fatal(out.String())
	}
	next := g.Forwards.find(g.Forwards.reverseKey(d, "tcp:8080"))
	g.Forwards.removeRule(context.Background(), g.Forwards.reverseKey(d, "tcp:8080"), old)
	if g.Forwards.find(g.Forwards.reverseKey(d, "tcp:8080")) != next {
		t.Fatal("stale cleanup removed replacement")
	}
	g.Forwards.Drop(s.Tunnel)
	if g.Forwards.find(g.Forwards.reverseKey(d, "tcp:8080")) != next {
		t.Fatal("old tunnel removed replacement")
	}
}
func TestForwardBindFailurePreservesRule(t *testing.T) {
	g, _, _ := forwardingFixture(t)
	s := scopeA()
	d := selectDevice(t, g.Engine, s, "A")
	var out bytes.Buffer
	g.Forwards.Forward(context.Background(), &out, s, d, "tcp:8080;tcp:80")
	old := g.Forwards.find(g.Forwards.key(s, "tcp:8080"))
	p := newTestPeer()
	p.request = func(Control) error { return errors.New("address in use") }
	g.Forwards.Attach(s.Tunnel, p)
	out.Reset()
	g.Forwards.Forward(context.Background(), &out, s, d, "tcp:8080;tcp:90")
	if !strings.HasPrefix(out.String(), "FAIL") || g.Forwards.find(g.Forwards.key(s, "tcp:8080")) != old {
		t.Fatal("bind failure destroyed previous rule")
	}
}

func TestForwardActivationFailureRestoresPreviousRule(t *testing.T) {
	g, _, _ := forwardingFixture(t)
	s := scopeA()
	d := selectDevice(t, g.Engine, s, "A")
	var out bytes.Buffer
	g.Forwards.Forward(context.Background(), &out, s, d, "tcp:8080;tcp:80")
	key := g.Forwards.key(s, "tcp:8080")
	old := g.Forwards.find(key)
	p := newTestPeer()
	p.request = func(m Control) error {
		if m.Op == "activate" {
			return errors.New("activation failed")
		}
		return nil
	}
	g.Forwards.Attach(s.Tunnel, p)
	out.Reset()
	g.Forwards.Forward(context.Background(), &out, s, d, "tcp:8080;tcp:90")
	if !strings.HasPrefix(out.String(), "FAIL") || g.Forwards.find(key) != old {
		t.Fatal("activation failure lost original rule")
	}
}
