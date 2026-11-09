package gateway

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type gatedBackendConn struct {
	net.Conn
	entered, allow chan struct{}
	once           sync.Once
}

func (c *gatedBackendConn) Close() error {
	c.once.Do(func() { close(c.entered); <-c.allow; c.Conn.Close() })
	return nil
}

func TestWriterHandoffWaitsForBackendAndCancelsPendingCommand(t *testing.T) {
	e := engineFixture(t)
	first, err := e.CreateLease(scopeA().Identity, "a", "A", time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	selected, proceed := make(chan struct{}), make(chan struct{})
	closing, allowClose := make(chan struct{}), make(chan struct{})
	backendDone, command := make(chan struct{}), make(chan string, 1)
	var unblock, allow sync.Once
	t.Cleanup(func() { allow.Do(func() { close(allowClose) }); unblock.Do(func() { close(proceed) }) })
	g := NewGateway(e, Backend{Dial: func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer close(backendDone)
			defer b.Close()
			if _, err := ReadFrame(b); err != nil {
				return
			}
			close(selected)
			<-proceed
			if okay(b) != nil {
				return
			}
			if req, err := ReadFrame(b); err == nil {
				command <- req
				okay(b)
			}
		}()
		return &gatedBackendConn{Conn: a, entered: closing, allow: allowClose}, nil
	}})
	a, b := net.Pipe()
	defer a.Close()
	s := scopeA()
	s.Lease = first.ID
	go g.Handle(context.Background(), b, s)
	a.SetDeadline(time.Now().Add(3 * time.Second))
	if err := WriteFrame(a, "host:transport:A"); err != nil {
		t.Fatal(err)
	}
	if err := readStatus(a); err != nil {
		t.Fatal(err)
	}
	if err := WriteFrame(a, "reboot:"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-selected:
	case <-time.After(2 * time.Second):
		t.Fatal("backend selection did not start")
	}
	next, err := e.CreateLease(scopeB().Identity, "b", "A", time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.ReleaseLease(first.ID, s.Identity); err != nil {
		t.Fatal(err)
	}
	select {
	case <-closing:
	case <-time.After(2 * time.Second):
		t.Fatal("backend was not canceled")
	}
	if lease, _ := e.Lease(next.ID); lease.State != "queued" {
		t.Fatal("successor ran before backend closed")
	}
	other := getRead(t, e, scopeA(), "B", &testCloser{})
	other()
	allow.Do(func() { close(allowClose) })
	waitFor(t, func() bool { lease, _ := e.Lease(next.ID); return lease.State == "active" })
	unblock.Do(func() { close(proceed) })
	select {
	case <-backendDone:
	case <-time.After(2 * time.Second):
		t.Fatal("backend did not finish")
	}
	select {
	case req := <-command:
		t.Fatalf("revoked writer sent %q after handoff", req)
	default:
	}
}

func TestReverseRemovalFailureIsIsolatedAndRetried(t *testing.T) {
	g, backend, network := forwardingFixture(t)
	var offline atomic.Bool
	g.Backend.Dial = func(ctx context.Context) (net.Conn, error) {
		if offline.Load() {
			return nil, errors.New("temporary backend failure")
		}
		return backend.dial(ctx)
	}
	d := selectDevice(t, g.Engine, scopeA(), "A")
	var out bytes.Buffer
	g.Forwards.Reverse(context.Background(), &out, scopeA(), d, "forward:tcp:8080;tcp:18080")
	if out.String() != "OKAYOKAY" {
		t.Fatal(out.String())
	}
	old := g.Forwards.find(g.Forwards.reverseKey(d, "tcp:8080"))
	address := old.listener.Addr().String()
	backend.mu.Lock()
	stale := backend.reverse["tcp:8080"]
	backend.mu.Unlock()
	if !strings.HasPrefix(stale, "localabstract:adbgroup-") {
		t.Fatalf("reusable backend target %q", stale)
	}
	offline.Store(true)
	out.Reset()
	g.Forwards.Reverse(context.Background(), &out, scopeA(), d, "killforward:tcp:8080")
	if !strings.HasPrefix(out.String(), "FAIL") {
		t.Fatalf("failed cleanup reported success: %q", out.String())
	}
	if g.Forwards.valid(old) {
		t.Fatal("retiring reverse still accepts traffic")
	}
	if conn, err := network.dial(context.Background(), address); err == nil {
		conn.Close()
		t.Fatal("stale target remains accessible")
	}
	if g.Forwards.find(g.Forwards.reverseKey(d, old.local)) != old {
		t.Fatal("failed removal lost retry state")
	}
	offline.Store(false)
	deviceC := selectDevice(t, g.Engine, scopeB(), "C")
	out.Reset()
	g.Forwards.Reverse(context.Background(), &out, scopeB(), deviceC, "forward:tcp:8081;tcp:28080")
	if out.String() != "OKAYOKAY" {
		t.Fatal(out.String())
	}
	current := g.Forwards.find(g.Forwards.reverseKey(deviceC, "tcp:8081"))
	if current.listener.Addr().String() == address {
		t.Fatal("another group reused a stale endpoint")
	}
	if conn, err := network.dial(context.Background(), address); err == nil {
		conn.Close()
		t.Fatal("stale mapping reached another group")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	old.retryAt.Store(0)
	go g.Forwards.Sweep(ctx)
	waitFor(t, func() bool { return g.Forwards.find(g.Forwards.reverseKey(d, old.local)) == nil })
	backend.mu.Lock()
	_, remains := backend.reverse["tcp:8080"]
	_, unrelated := backend.reverse["tcp:8081"]
	backend.mu.Unlock()
	if remains || !unrelated {
		t.Fatal("cleanup failed or removed another group's mapping")
	}
}

func TestReverseLostRemovalReplyIsConfirmed(t *testing.T) {
	g, backend, _ := forwardingFixture(t)
	d := selectDevice(t, g.Engine, scopeA(), "A")
	var out bytes.Buffer
	g.Forwards.Reverse(context.Background(), &out, scopeA(), d, "forward:tcp:8080;tcp:18080")
	if out.String() != "OKAYOKAY" {
		t.Fatal(out.String())
	}
	g.Backend.Dial = func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			if _, err := ReadFrame(b); err != nil {
				return
			}
			okay(b)
			req, err := ReadFrame(b)
			if err != nil {
				return
			}
			okay(b)
			if req == "reverse:killforward:tcp:8080" {
				backend.mu.Lock()
				delete(backend.reverse, "tcp:8080")
				backend.mu.Unlock()
				return // adbd applied the removal, but its acknowledgement was lost.
			}
			if req == "reverse:list-forward" {
				WriteFrame(b, "")
			}
		}()
		return a, nil
	}
	out.Reset()
	g.Forwards.Reverse(context.Background(), &out, scopeA(), d, "killforward:tcp:8080")
	if out.String() != "OKAYOKAY" {
		t.Fatalf("confirmed removal failed: %q", out.String())
	}
	if len(g.Forwards.snapshot()) != 0 {
		t.Fatal("confirmed removal retained retry state")
	}
}

func TestReverseLostCreationReplyDoesNotExposeLaterRules(t *testing.T) {
	g, backend, network := forwardingFixture(t)
	g.Backend.Dial = func(context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			if _, err := ReadFrame(b); err != nil {
				return
			}
			okay(b)
			req, err := ReadFrame(b)
			if err != nil {
				return
			}
			okay(b)
			local, remote, _, err := splitForward(strings.TrimPrefix(req, "reverse:forward:"))
			if err != nil {
				return
			}
			backend.mu.Lock()
			backend.reverse[local] = remote
			backend.mu.Unlock()
			// The device installed this mapping, but the operation's reply is lost.
		}()
		return a, nil
	}
	var out bytes.Buffer
	d := selectDevice(t, g.Engine, scopeA(), "A")
	g.Forwards.Reverse(context.Background(), &out, scopeA(), d, "forward:tcp:8080;tcp:18080")
	if !strings.HasPrefix(out.String(), "FAIL") {
		t.Fatal(out.String())
	}
	backend.mu.Lock()
	stale := backend.reverse["tcp:8080"]
	backend.mu.Unlock()
	if !strings.HasPrefix(stale, "localabstract:adbgroup-") {
		t.Fatal(stale)
	}
	g.Backend.Dial = backend.dial
	out.Reset()
	g.Forwards.Reverse(context.Background(), &out, scopeB(), selectDevice(t, g.Engine, scopeB(), "C"), "forward:tcp:8081;tcp:28080")
	if out.String() != "OKAYOKAY" {
		t.Fatal(out.String())
	}
	if conn, err := network.dial(context.Background(), "@"+strings.TrimPrefix(stale, "localabstract:")); err == nil {
		conn.Close()
		t.Fatal("unacknowledged creation reached a later rule")
	}
}
