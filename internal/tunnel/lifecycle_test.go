package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

func TestClientAcceptsAfterConnectionLimitDrains(t *testing.T) {
	h := newHarness(t)
	h.start(t, "")
	var conns []net.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < 128; i++ {
		c, err := h.local.dial(h.server.ctx, "127.0.0.1:15037")
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		want := i + 1
		wait(t, func() bool { h.client.mu.Lock(); defer h.client.mu.Unlock(); return len(h.client.connections) == want })
	}
	overflow, err := h.local.dial(h.server.ctx, "127.0.0.1:15037")
	if err != nil {
		t.Fatal(err)
	}
	overflow.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := overflow.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("overflow was not promptly rejected: %v", err)
	}
	overflow.Close()
	for _, c := range conns {
		c.Close()
	}
	wait(t, func() bool { h.client.mu.Lock(); defer h.client.mu.Unlock(); return len(h.client.connections) == 0 })
	c, err := h.local.dial(h.server.ctx, "127.0.0.1:15037")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if err := gateway.WriteFrame(c, "host:devices"); err != nil {
		t.Fatalf("listener did not recover: %v", err)
	}
	status(t, c)
	devices, err := gateway.ReadFrame(c)
	if err != nil || !strings.Contains(devices, "A\tdevice") {
		t.Fatalf("recovered listener returned %q: %v", devices, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func reverseCancellationFixture(t *testing.T) (*harness, *session, string, <-chan struct{}) {
	t.Helper()
	h := newHarness(t)
	h.start(t, "")
	app, err := h.local.listen("tcp", "127.0.0.1:18080")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.Close() })
	appClosed := make(chan struct{})
	go func() {
		c, err := app.Accept()
		if err != nil {
			return
		}
		defer close(appClosed)
		defer c.Close()
		io.Copy(io.Discard, c)
	}()
	c := h.adb(t, "reverse:forward:tcp:8080;tcp:18080")
	status(t, c)
	c.Close()
	h.server.mu.Lock()
	var owner *session
	for _, s := range h.server.sessions {
		owner = s
	}
	h.server.mu.Unlock()
	wait(t, func() bool { owner.mu.Lock(); defer owner.mu.Unlock(); return len(owner.streams) == 0 })
	wait(t, func() bool { h.client.mu.Lock(); defer h.client.mu.Unlock(); return len(h.client.connections) == 0 })
	h.client.mu.Lock()
	var rule string
	for k := range h.client.reverse {
		rule = k
	}
	h.client.mu.Unlock()
	return h, owner, rule, appClosed
}

func TestReverseCancellationClosesAttachedStreamAndClientSocket(t *testing.T) {
	h, owner, rule, appClosed := reverseCancellationFixture(t)
	ctx, cancel := context.WithCancel(h.server.ctx)
	defer cancel()
	base := h.client.http.Transport
	var attached atomic.Bool
	// Cancel the device's callback after its WebSocket is attached but before
	// the client can reply to the control request. TLS and WebSocket are real.
	h.client.http.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		res, err := base.RoundTrip(r)
		if err != nil || res.StatusCode != http.StatusSwitchingProtocols || !strings.Contains(r.URL.Path, "/streams/") {
			return res, err
		}
		until := time.Now().Add(2 * time.Second)
		for time.Now().Before(until) {
			owner.mu.Lock()
			v := owner.streams[path.Base(r.URL.Path)]
			ready := v != nil && v.kind == "reverse" && v.conn != nil
			owner.mu.Unlock()
			if ready {
				attached.Store(true)
				cancel()
				return res, nil
			}
			time.Sleep(time.Millisecond)
		}
		res.Body.Close()
		return nil, errors.New("reverse stream did not attach")
	})
	conn, err := owner.Reverse(ctx, rule, 18080)
	if conn != nil {
		conn.Close()
		t.Fatal("canceled reverse returned a connection")
	}
	if !attached.Load() || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation did not cover attached stream: %v", err)
	}
	wait(t, func() bool { owner.mu.Lock(); defer owner.mu.Unlock(); return len(owner.streams) == 0 })
	wait(t, func() bool { h.client.mu.Lock(); defer h.client.mu.Unlock(); return len(h.client.connections) == 0 })
	select {
	case <-appClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("client target socket leaked")
	}
}

func TestReverseCancellationRejectsLateAttachment(t *testing.T) {
	h, owner, rule, appClosed := reverseCancellationFixture(t)
	started, proceed := make(chan struct{}), make(chan struct{})
	var unblock sync.Once
	t.Cleanup(func() { unblock.Do(func() { close(proceed) }) })
	dial := h.client.dial
	h.client.dial = func(ctx context.Context, address string) (net.Conn, error) {
		close(started)
		select {
		case <-proceed:
			return dial(ctx, address)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	ctx, cancel := context.WithCancel(h.server.ctx)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		conn, err := owner.Reverse(ctx, rule, 18080)
		if conn != nil {
			conn.Close()
			err = errors.New("canceled reverse returned a connection")
		}
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("client did not start the callback")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("callback cancellation: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback cancellation waited for the client reply")
	}
	owner.mu.Lock()
	remaining := len(owner.streams)
	owner.mu.Unlock()
	if remaining != 0 {
		t.Fatal("canceled stream remains attachable")
	}
	unblock.Do(func() { close(proceed) })
	select {
	case <-appClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("late attachment leaked the client target socket")
	}
	wait(t, func() bool { h.client.mu.Lock(); defer h.client.mu.Unlock(); return len(h.client.connections) == 0 })
	healthy := h.adb(t, "exec:echo hello")
	defer healthy.Close()
	data, err := io.ReadAll(healthy)
	if err != nil || string(data) != "hello\n" {
		t.Fatalf("late reply damaged the tunnel: %q %v", data, err)
	}
}
