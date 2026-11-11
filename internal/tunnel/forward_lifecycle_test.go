package tunnel

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

func forwardDevice(t *testing.T, h *harness, serial, spec string) {
	t.Helper()
	c := h.adbDevice(t, serial, "host:forward:"+spec)
	defer c.Close()
	status(t, c)
}

func forwardConn(t *testing.T, h *harness, address string) net.Conn {
	t.Helper()
	c, err := h.local.dial(h.server.ctx, address)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	echo(t, c)
	return c
}

func clientListener(t *testing.T, h *harness, port int) net.Listener {
	t.Helper()
	h.client.mu.Lock()
	defer h.client.mu.Unlock()
	for _, v := range h.client.listeners {
		if v.listener.Addr().(*net.TCPAddr).Port == port {
			return v.listener
		}
	}
	t.Fatalf("missing listener for %d", port)
	return nil
}

func forwardList(t *testing.T, h *harness) string {
	t.Helper()
	c, err := h.local.dial(h.server.ctx, "127.0.0.1:15037")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if err := gateway.WriteFrame(c, "host:list-forward"); err != nil {
		t.Fatal(err)
	}
	status(t, c)
	list, err := gateway.ReadFrame(c)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func TestForwardHotplugClearsOnlyDisconnectedDevice(t *testing.T) {
	h := newHarness(t)
	h.start(t, "")
	forwardDevice(t, h, "A", "tcp:8080;tcp:80")
	forwardDevice(t, h, "B", "tcp:8081;tcp:81")
	a := forwardConn(t, h, "127.0.0.1:8080")
	b := forwardConn(t, h, "127.0.0.1:8081")
	h.engine.Update([]gateway.Device{{Serial: "B", State: "device", TransportID: "2"}})
	if list := forwardList(t, h); strings.Contains(list, "tcp:8080") || !strings.Contains(list, "B tcp:8081 tcp:81") {
		t.Fatalf("disconnected device still listed or another device lost: %q", list)
	}
	a.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := a.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("disconnected device's forward stayed open: %v", err)
	}
	wait(t, func() bool {
		h.client.mu.Lock()
		defer h.client.mu.Unlock()
		return len(h.client.listeners) == 1
	})
	h.engine.Update([]gateway.Device{{Serial: "A", State: "device", TransportID: "9"}, {Serial: "B", State: "device", TransportID: "2"}})
	if list := forwardList(t, h); strings.Contains(list, "tcp:8080") {
		t.Fatalf("replug restored an old forward: %q", list)
	}
	if conn, err := h.local.dial(h.server.ctx, "127.0.0.1:8080"); err == nil {
		conn.Close()
		t.Fatal("replug restored the old listener")
	}
	echo(t, b)
}

func TestForwardNewGenerationUsesNewListener(t *testing.T) {
	h := newHarnessWithSweep(t, false)
	h.start(t, "")
	forwardDevice(t, h, "A", "tcp:8080;tcp:80")
	forwardDevice(t, h, "B", "tcp:8081;tcp:81")
	old := clientListener(t, h, 8080)
	b := forwardConn(t, h, "127.0.0.1:8081")
	h.engine.Update([]gateway.Device{{Serial: "A", State: "device", TransportID: "9"}, {Serial: "B", State: "device", TransportID: "2"}})
	forwardDevice(t, h, "A", "norebind:tcp:8080;tcp:90")
	if clientListener(t, h, 8080) == old {
		t.Fatal("new device generation reused the old listener")
	}
	forwardConn(t, h, "127.0.0.1:8080")
	echo(t, b)
}

type forwardRemovalGate struct {
	gateway.Peer
	rule              string
	removing, proceed chan struct{}
	once              sync.Once
}

func (p *forwardRemovalGate) Request(ctx context.Context, m gateway.Control) (gateway.Control, error) {
	if m.Op == "remove" && m.Rule == p.rule {
		p.once.Do(func() { close(p.removing) })
		select {
		case <-p.proceed:
		case <-ctx.Done():
			return gateway.Control{}, ctx.Err()
		}
	}
	return p.Peer.Request(ctx, m)
}

func TestForwardRecreateWaitsForOldCleanup(t *testing.T) {
	h := newHarnessWithSweep(t, false)
	h.start(t, "")
	forwardDevice(t, h, "A", "tcp:8080;tcp:80")
	forwardDevice(t, h, "B", "tcp:8081;tcp:81")
	old := clientListener(t, h, 8080)
	b := forwardConn(t, h, "127.0.0.1:8081")
	h.server.mu.Lock()
	var owner *session
	for _, s := range h.server.sessions {
		owner = s
	}
	h.server.mu.Unlock()
	h.client.mu.Lock()
	var rule string
	for id, v := range h.client.listeners {
		if v.listener == old {
			rule = id
		}
	}
	h.client.mu.Unlock()
	p := &forwardRemovalGate{Peer: owner, rule: rule, removing: make(chan struct{}), proceed: make(chan struct{})}
	h.gateway.Forwards.Attach(owner.ID, p)
	var allow sync.Once
	t.Cleanup(func() { allow.Do(func() { close(p.proceed) }) })
	h.engine.Update([]gateway.Device{{Serial: "A", State: "device", TransportID: "9"}, {Serial: "B", State: "device", TransportID: "2"}})
	go h.gateway.Forwards.Sweep(h.server.ctx)
	select {
	case <-p.removing:
	case <-time.After(3 * time.Second):
		t.Fatal("old generation cleanup did not start")
	}
	c, err := h.local.dial(h.server.ctx, "127.0.0.1:15037")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	gateway.WriteFrame(c, "host:transport:A")
	status(t, c)
	gateway.WriteFrame(c, "host:forward:norebind:tcp:8080;tcp:90")
	c.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("new forward responded before the old listener was removed")
	} else if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("forward failed while awaiting cleanup: %v", err)
	}
	// A held cleanup must not stop data flowing through another device.
	echo(t, b)
	allow.Do(func() { close(p.proceed) })
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	status(t, c)
	status(t, c)
	if clientListener(t, h, 8080) == old {
		t.Fatal("cleanup reused the old generation's listener")
	}
	forwardConn(t, h, "127.0.0.1:8080")
	echo(t, b)
}
