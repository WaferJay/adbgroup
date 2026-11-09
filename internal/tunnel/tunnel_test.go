package tunnel

import (
	"adbgroup/internal/gateway"
	"adbgroup/internal/pki"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Memory sockets exercise real TLS, HTTP upgrades, framing, and backpressure
// without requiring kernel TCP listener permissions.
type memoryNetwork struct {
	mu        sync.Mutex
	listeners map[string]*memoryListener
	next      int
}
type memoryListener struct {
	n        *memoryNetwork
	addr     net.Addr
	incoming chan net.Conn
	done     chan struct{}
	once     sync.Once
}

func memory() *memoryNetwork {
	return &memoryNetwork{listeners: map[string]*memoryListener{}, next: 30000}
}
func (n *memoryNetwork) listen(network, address string) (net.Listener, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	var a net.Addr
	if network == "unix" {
		a = &net.UnixAddr{Name: address, Net: "unix"}
	} else {
		tcp, err := net.ResolveTCPAddr("tcp", address)
		if err != nil {
			return nil, err
		}
		if tcp.Port == 0 {
			n.next++
			tcp.Port = n.next
		}
		a = tcp
	}
	if n.listeners[a.String()] != nil {
		return nil, fmt.Errorf("address in use")
	}
	l := &memoryListener{n: n, addr: a, incoming: make(chan net.Conn, 32), done: make(chan struct{})}
	n.listeners[a.String()] = l
	return l, nil
}
func (n *memoryNetwork) dial(ctx context.Context, address string) (net.Conn, error) {
	n.mu.Lock()
	l := n.listeners[address]
	n.mu.Unlock()
	if l == nil {
		return nil, fmt.Errorf("connection refused: %s", address)
	}
	a, b := net.Pipe()
	select {
	case l.incoming <- b:
		return a, nil
	case <-ctx.Done():
		a.Close()
		b.Close()
		return nil, ctx.Err()
	case <-l.done:
		a.Close()
		b.Close()
		return nil, net.ErrClosed
	}
}
func (l *memoryListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.incoming:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *memoryListener) Close() error {
	l.once.Do(func() { l.n.mu.Lock(); delete(l.n.listeners, l.addr.String()); l.n.mu.Unlock(); close(l.done) })
	return nil
}
func (l *memoryListener) Addr() net.Addr { return l.addr }

type backendFixture struct {
	mu      sync.Mutex
	reverse map[string]string
}

func (b *backendFixture) dial(context.Context) (net.Conn, error) {
	a, c := net.Pipe()
	go func() {
		defer c.Close()
		for {
			req, err := gateway.ReadFrame(c)
			if err != nil {
				return
			}
			io.WriteString(c, "OKAY")
			if strings.HasPrefix(req, "host:transport-id:") {
				continue
			}
			if strings.HasPrefix(req, "tcp:") {
				io.Copy(c, c)
				return
			}
			if strings.HasPrefix(req, "exec:") {
				io.WriteString(c, "hello\n")
				return
			}
			if strings.HasPrefix(req, "reverse:forward:") {
				parts := strings.SplitN(strings.TrimPrefix(req, "reverse:forward:"), ";", 2)
				b.mu.Lock()
				b.reverse[parts[0]] = parts[1]
				b.mu.Unlock()
				io.WriteString(c, "OKAY")
				return
			}
			if strings.HasPrefix(req, "reverse:killforward:") {
				b.mu.Lock()
				delete(b.reverse, strings.TrimPrefix(req, "reverse:killforward:"))
				b.mu.Unlock()
				io.WriteString(c, "OKAY")
				return
			}
			return
		}
	}()
	return a, nil
}

type harness struct {
	engine      *gateway.Engine
	gateway     *gateway.Gateway
	server      *Server
	client      *Client
	host, local *memoryNetwork
	backend     *backendFixture
	cancel      context.CancelFunc
	done        chan error
	issuer      pki.Files
}

func newHarness(t *testing.T) *harness {
	return newHarnessWithSweep(t, true)
}
func newHarnessWithSweep(t *testing.T, startSweep bool) *harness {
	t.Helper()
	dir := t.TempDir()
	files, err := pki.Ensure(dir+"/pki", []string{"adb.test"})
	if err != nil {
		t.Fatal(err)
	}
	user, err := pki.IssueClient(files.CA, files.CAKey, "spiffe://adbgroup/alice", dir+"/alice", 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateway.Config{Groups: []gateway.Group{{Name: "a", Devices: []string{"A", "B"}}}, Identities: map[string][]string{"spiffe://adbgroup/alice": {"a"}}}
	cfg.Defaults()
	e := gateway.NewEngine(cfg)
	e.Update([]gateway.Device{{Serial: "A", State: "device", TransportID: "1"}, {Serial: "B", State: "device", TransportID: "2"}})
	b := &backendFixture{reverse: map[string]string{}}
	g := gateway.NewGateway(e, gateway.Backend{Dial: b.dial})
	host, local := memory(), memory()
	g.Forwards.Listen = host.listen
	ctx, cancel := context.WithCancel(context.Background())
	s := NewServer(ctx, g)
	pair, err := tls.LoadX509KeyPair(files.Certificate, files.Key)
	if err != nil {
		t.Fatal(err)
	}
	pem, err := os.ReadFile(files.CA)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(pem)
	httpServer := &http.Server{Handler: (gateway.API{Engine: e, Tunnels: s.Handler()}).Handler(), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}}
	l, err := host.listen("tcp", "127.0.0.1:8443")
	if err != nil {
		t.Fatal(err)
	}
	go httpServer.ServeTLS(l, "", "")
	go e.Run(ctx)
	if startSweep {
		go g.Forwards.Sweep(ctx)
	}
	c, err := NewClient("https://adb.test:8443", user.Certificate, user.Key, user.CA)
	if err != nil {
		t.Fatal(err)
	}
	c.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return host.dial(ctx, "127.0.0.1:8443") }
	c.listen = local.listen
	c.dial = local.dial
	h := &harness{e, g, s, c, host, local, b, cancel, make(chan error, 1), files}
	t.Cleanup(func() { cancel(); s.Close(); httpServer.Close(); g.Forwards.Close() })
	return h
}
func wait(t *testing.T, p func() bool) {
	t.Helper()
	until := time.Now().Add(3 * time.Second)
	for !p() {
		if time.Now().After(until) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}
func (h *harness) start(t *testing.T, lease string) {
	t.Helper()
	go func() { h.done <- h.client.Run(h.server.ctx, "a", lease, "127.0.0.1:15037") }()
	wait(t, func() bool {
		h.local.mu.Lock()
		defer h.local.mu.Unlock()
		return h.local.listeners["127.0.0.1:15037"] != nil
	})
	t.Cleanup(func() {
		h.cancel()
		select {
		case err := <-h.done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(3 * time.Second):
			t.Error("client did not stop")
		}
	})
}
func status(t *testing.T, c net.Conn) {
	t.Helper()
	var b [4]byte
	if _, err := io.ReadFull(c, b[:]); err != nil || string(b[:]) != "OKAY" {
		t.Fatalf("status %q: %v", b, err)
	}
}
func (h *harness) adb(t *testing.T, service string) net.Conn {
	return h.adbDevice(t, "A", service)
}
func (h *harness) adbDevice(t *testing.T, serial, service string) net.Conn {
	t.Helper()
	c, err := h.local.dial(h.server.ctx, "127.0.0.1:15037")
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	gateway.WriteFrame(c, "host:transport:"+serial)
	status(t, c)
	gateway.WriteFrame(c, service)
	status(t, c)
	return c
}
func echo(t *testing.T, c net.Conn) {
	t.Helper()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	io.WriteString(c, "payload")
	var b [7]byte
	if _, err := io.ReadFull(c, b[:]); err != nil || string(b[:]) != "payload" {
		t.Fatalf("echo %q %v", b, err)
	}
}
func TestMTLSTunnelADBForwardReverseAndCleanup(t *testing.T) {
	h := newHarness(t)
	h.start(t, "")
	c := h.adb(t, "exec:echo hello")
	out, err := io.ReadAll(c)
	c.Close()
	if err != nil || string(out) != "hello\n" {
		t.Fatalf("exec %q %v", out, err)
	}
	c = h.adb(t, "host:forward:tcp:0;tcp:80")
	status(t, c)
	port, err := gateway.ReadFrame(c)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	data, err := h.local.dial(h.server.ctx, "127.0.0.1:"+port)
	if err != nil {
		t.Fatal(err)
	}
	echo(t, data)
	lease, err := h.engine.CreateLease("spiffe://adbgroup/alice", "a", "A", time.Second, time.Minute)
	if err != nil || lease.State != "queued" {
		t.Fatalf("lock ignored forward stream %+v %v", lease, err)
	}
	data.Close()
	wait(t, func() bool { l, _ := h.engine.Lease(lease.ID); return l.State == "active" })
	h.engine.ReleaseLease(lease.ID, "spiffe://adbgroup/alice")
	app, err := h.local.listen("tcp", "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	defer app.Close()
	go func() {
		raw, e := app.Accept()
		if e == nil {
			defer raw.Close()
			io.Copy(raw, raw)
		}
	}()
	c = h.adb(t, "reverse:forward:tcp:8080;tcp:18080")
	status(t, c)
	c.Close()
	h.backend.mu.Lock()
	target := h.backend.reverse["tcp:8080"]
	h.backend.mu.Unlock()
	if !strings.HasPrefix(target, "localabstract:adbgroup-") {
		t.Fatalf("unsafe reverse target: %q", target)
	}
	reverse, err := h.host.dial(h.server.ctx, "@"+strings.TrimPrefix(target, "localabstract:"))
	if err != nil {
		t.Fatal(err)
	}
	echo(t, reverse)
	reverse.Close()
	// Losing the authenticated control channel closes local listeners and removes
	// only this tunnel's device-side mapping.
	h.server.mu.Lock()
	var session *session
	for _, v := range h.server.sessions {
		session = v
	}
	h.server.mu.Unlock()
	session.close()
	<-session.cleaned
	wait(t, func() bool {
		h.local.mu.Lock()
		defer h.local.mu.Unlock()
		return h.local.listeners["127.0.0.1:"+port] == nil
	})
	h.backend.mu.Lock()
	remaining := len(h.backend.reverse)
	h.backend.mu.Unlock()
	if remaining != 0 {
		t.Fatal("reverse survived tunnel close")
	}
	select {
	case <-h.done:
		h.done <- nil
	case <-time.After(3 * time.Second):
		t.Fatal("control closure did not stop client")
	}
}
func TestTunnelResourceAuthorizationAndLeaseBinding(t *testing.T) {
	h := newHarness(t)
	c := h.client
	var v Resource
	ctx := h.server.ctx
	if err := c.request(ctx, "POST", "/v1/groups/a/tunnels", map[string]string{}, &v); err != nil {
		t.Fatal(err)
	}
	if err := c.request(ctx, "POST", "/v1/groups/missing/tunnels", map[string]string{}, &Resource{}); err == nil {
		t.Fatal("unknown group accepted")
	}
	if err := c.request(ctx, "POST", "/v1/groups/a/tunnels", map[string]string{"lease_id": "invalid"}, &Resource{}); err == nil {
		t.Fatal("unknown lease accepted")
	}
	if err := c.request(ctx, "POST", v.Path+"/streams", map[string]string{"kind": "adb"}, &Resource{}); err == nil {
		t.Fatal("stream accepted without control")
	}
	if err := c.request(ctx, "DELETE", v.Path, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := c.request(ctx, "DELETE", v.Path, nil, nil); err != nil {
		t.Fatal("DELETE not idempotent", err)
	}
	var state Resource
	if err := c.request(ctx, "GET", v.Path, nil, &state); err != nil || state.State != "closed" {
		t.Fatalf("closed resource %+v %v", state, err)
	}
	// Wrong server name must fail TLS, even with the correct client certificate.
	c.base.Host = "wrong.test:8443"
	if err := c.request(ctx, "GET", "/v1/groups", nil, &json.RawMessage{}); err == nil {
		t.Fatal("TLS server name not checked")
	}
}

func TestMTLSRevocationAndHotplugAreScoped(t *testing.T) {
	h := newHarness(t)
	h.start(t, "")
	c := h.adb(t, "tcp:80")
	defer c.Close()
	echo(t, c)
	// Unplug B, leaving A's generation and live connection intact.
	current := h.engine.Snapshot(gateway.Scope{Group: "a", Identity: "spiffe://adbgroup/alice"})
	h.engine.Update(current[:1])
	echo(t, c)
	cfg := h.engine.Config()
	cfg.Identities = map[string][]string{}
	h.engine.Reload(cfg)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("stream survived revocation")
	}
	wait(t, func() bool {
		h.local.mu.Lock()
		defer h.local.mu.Unlock()
		return h.local.listeners["127.0.0.1:15037"] == nil
	})
	select {
	case <-h.done:
		h.done <- nil
	case <-time.After(3 * time.Second):
		t.Fatal("revocation did not stop client")
	}
}

func TestWriteTunnelAndSingleUseStreams(t *testing.T) {
	h := newHarness(t)
	lease, err := h.engine.CreateLease("spiffe://adbgroup/alice", "a", "A", time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	h.start(t, lease.ID)
	c := h.adb(t, "tcp:80")
	echo(t, c)
	c.Close()
	var v Resource
	if err := h.client.request(h.server.ctx, "POST", h.client.path+"/streams", map[string]string{"kind": "adb"}, &v); err != nil {
		t.Fatal(err)
	}
	ws, err := h.client.websocket(h.server.ctx, v.Path)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.CloseNow()
	if duplicate, err := h.client.websocket(h.server.ctx, v.Path); err == nil {
		duplicate.CloseNow()
		t.Fatal("stream reused")
	}
	// A separate identity may be granted the same group, but not Alice's lease.
	cfg := h.engine.Config()
	cfg.Identities = map[string][]string{"spiffe://adbgroup/alice": {"a"}, "spiffe://adbgroup/bob": {"a"}}
	h.engine.Reload(cfg)
	if h.engine.AuthorizedScope(gateway.Scope{Group: "a", Identity: "spiffe://adbgroup/bob", Lease: lease.ID}) {
		t.Fatal("lease stolen")
	}
	h.engine.ReleaseLease(lease.ID, "spiffe://adbgroup/alice")
	wait(t, func() bool {
		h.local.mu.Lock()
		defer h.local.mu.Unlock()
		return h.local.listeners["127.0.0.1:15037"] == nil
	})
	select {
	case <-h.done:
		h.done <- nil
	case <-time.After(3 * time.Second):
		t.Fatal("lease expiry did not stop client")
	}
}

func TestClientRejectsUnauthenticatedTLSAndPublicListen(t *testing.T) {
	h := newHarness(t)
	if err := localAddress("0.0.0.0:15037"); err == nil {
		t.Fatal("public client listener accepted")
	}
	tr := h.client.http.Transport.(*http.Transport)
	tr.TLSClientConfig.Certificates = nil
	if err := h.client.request(h.server.ctx, "GET", "/v1/groups", nil, &json.RawMessage{}); err == nil {
		t.Fatal("missing certificate accepted")
	}
}

func TestCertificateExpiryClosesExistingTunnel(t *testing.T) {
	h := newHarness(t)
	h.start(t, "")
	h.server.mu.Lock()
	s := h.server.sessions[strings.TrimPrefix(h.client.path, "/v1/groups/a/tunnels/")]
	h.server.mu.Unlock()
	s.mu.Lock()
	s.expires = time.Now().Add(-time.Second)
	s.mu.Unlock()
	wait(t, func() bool {
		h.local.mu.Lock()
		defer h.local.mu.Unlock()
		return h.local.listeners["127.0.0.1:15037"] == nil
	})
	select {
	case <-h.done:
		h.done <- nil
	case <-time.After(3 * time.Second):
		t.Fatal("expired certificate did not close tunnel")
	}
}

func TestUnknownIdentityAndCrossOwnerAccessRejected(t *testing.T) {
	h := newHarness(t)
	var v Resource
	if err := h.client.request(h.server.ctx, "POST", "/v1/groups/a/tunnels", map[string]string{}, &v); err != nil {
		t.Fatal(err)
	}
	cfg := h.engine.Config()
	cfg.Identities = map[string][]string{"spiffe://adbgroup/alice": {"a"}, "spiffe://adbgroup/bob": {"a"}}
	h.engine.Reload(cfg)

	for _, identity := range []string{"bob", "nobody"} {
		out := h.issuer.CA + "-" + identity
		user, err := pki.IssueClient(h.issuer.CA, h.issuer.CAKey, "spiffe://adbgroup/"+identity, out, 1)
		if err != nil {
			t.Fatal(err)
		}
		other, err := NewClient("https://adb.test:8443", user.Certificate, user.Key, user.CA)
		if err != nil {
			t.Fatal(err)
		}
		other.http.Transport.(*http.Transport).DialContext = h.client.http.Transport.(*http.Transport).DialContext
		err = other.request(h.server.ctx, "GET", v.Path, nil, &Resource{})
		other.http.CloseIdleConnections()
		expected := "404"
		if identity == "nobody" {
			expected = "403"
		}
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatalf("%s access: %v", identity, err)
		}
	}

}
