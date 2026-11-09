package gateway

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type fixtureBackend struct {
	l        net.Listener
	mu       sync.Mutex
	requests []string
	reverse  map[string]string
	next     int
}

func fakeBackend(t *testing.T) *fixtureBackend {
	t.Helper()
	return &fixtureBackend{reverse: map[string]string{}, next: 30000}
}
func (b *fixtureBackend) dial(context.Context) (net.Conn, error) {
	a, c := net.Pipe()
	go b.handle(c)
	return a, nil
}
func (b *fixtureBackend) handle(c net.Conn) {
	defer c.Close()
	for {
		req, e := ReadFrame(c)
		if e != nil {
			return
		}
		b.mu.Lock()
		b.requests = append(b.requests, req)
		b.mu.Unlock()
		switch {
		case req == "host:version":
			okay(c)
			WriteFrame(c, "0029")
			return
		case strings.HasSuffix(req, ":features") || req == "host:host-features":
			okay(c)
			WriteFrame(c, "")
			return
		case strings.HasPrefix(req, "host:transport-id:"):
			okay(c)
		case strings.HasPrefix(req, "reverse:"):
			okay(c)
			cmd := strings.TrimPrefix(req, "reverse:")
			b.mu.Lock()
			if strings.HasPrefix(cmd, "forward:") {
				local, remote, norebind, _ := splitForward(strings.TrimPrefix(cmd, "forward:"))
				allocated := local == "tcp:0"
				if allocated {
					b.next++
					local = "tcp:" + strconv.Itoa(b.next)
				}
				if b.reverse[local] != "" && norebind {
					fail(c, fmt.Errorf("cannot rebind"))
				} else {
					b.reverse[local] = remote
					okay(c)
					if allocated {
						WriteFrame(c, strings.TrimPrefix(local, "tcp:"))
					}
				}
			} else if strings.HasPrefix(cmd, "killforward:") {
				delete(b.reverse, strings.TrimPrefix(cmd, "killforward:"))
				okay(c)
			}
			b.mu.Unlock()
			return
		case strings.HasPrefix(req, "exec:") || strings.HasPrefix(req, "shell:"):
			okay(c)
			io.WriteString(c, "fixture-output\n")
			return
		case strings.HasPrefix(req, "tcp:"):
			okay(c)
			io.Copy(c, c)
			return
		default:
			fail(c, fmt.Errorf("unsupported fixture request %s", req))
			return
		}
	}
}
func gatewayFixture(t *testing.T) (*Gateway, *Gateway) {
	t.Helper()
	b := fakeBackend(t)
	e := engineFixture(t)
	g := NewGateway(e, Backend{Dial: b.dial})
	g.Forwards.Attach("a", newTestPeer())
	t.Cleanup(func() { g.Forwards.Close() })
	return g, g
}
func request(t *testing.T, g *Gateway, req string) net.Conn {
	t.Helper()
	c, server := net.Pipe()
	go g.Handle(context.Background(), server, scopeA())
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if e := WriteFrame(c, req); e != nil {
		t.Fatal(e)
	}
	return c
}
func TestProtocolIsolationAndBlocking(t *testing.T) {
	g, address := gatewayFixture(t)
	c := request(t, address, "host:devices-l")
	if err := readStatus(c); err != nil {
		t.Fatal(err)
	}
	list, err := ReadFrame(c)
	c.Close()
	if err != nil || !strings.Contains(list, "A\tdevice") || strings.Contains(list, "C\t") {
		t.Fatalf("bad list %q %v", list, err)
	}
	for _, req := range []string{"host:transport:C", "host:transport-id:3", "host-serial:C:features", "host:kill", "host:connect:1.2.3.4:5555"} {
		c = request(t, address, req)
		if readStatus(c) == nil {
			t.Fatalf("request escaped policy: %s", req)
		}
		c.Close()
	}
	l, _ := g.Engine.CreateLease("spiffe://test/alice", "a", "A", time.Second, time.Minute)
	c = request(t, address, "host:transport:A")
	result := make(chan error, 1)
	go func() { result <- readStatus(c) }()
	select {
	case err := <-result:
		t.Fatalf("reader bypassed writer %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	g.Engine.ReleaseLease(l.ID, "spiffe://test/alice")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	WriteFrame(c, "exec:echo")
	if err := readStatus(c); err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(c)
	c.Close()
	if string(out) != "fixture-output\n" {
		t.Fatalf("bad output %q", out)
	}
}
func TestWaitingClientDisconnectCancelsRead(t *testing.T) {
	g, address := gatewayFixture(t)
	l, _ := g.Engine.CreateLease("spiffe://test/alice", "a", "A", time.Second, time.Minute)
	defer g.Engine.ReleaseLease(l.ID, "spiffe://test/alice")
	c := request(t, address, "host:transport:A")
	deadline := time.Now().Add(time.Second)
	for selectDevice(t, g.Engine, scopeA(), "A").Queued == 0 {
		if time.Now().After(deadline) {
			t.Fatal("reader not queued")
		}
		time.Sleep(time.Millisecond)
	}
	c.Close()
	deadline = time.Now().Add(time.Second)
	for selectDevice(t, g.Engine, scopeA(), "A").Queued != 0 {
		if time.Now().After(deadline) {
			t.Fatal("disconnected reader leaked")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestModernADBTransportThenHostForward(t *testing.T) {
	g, _ := gatewayFixture(t)
	n := newMemNetwork()
	g.Forwards.Listen = n.listen

	c := request(t, g, "host:tport:serial:A")
	defer c.Close()
	if err := readStatus(c); err != nil {
		t.Fatal(err)
	}
	var id [8]byte
	if _, err := io.ReadFull(c, id[:]); err != nil || binary.LittleEndian.Uint64(id[:]) != 1 {
		t.Fatalf("bad tport response %v %v", id, err)
	}
	WriteFrame(c, "host:forward:tcp:0;tcp:80")
	if err := readStatus(c); err != nil {
		t.Fatal(err)
	}
	if err := readStatus(c); err != nil {
		t.Fatal(err)
	}
	port, err := ReadFrame(c)
	if err != nil || port != "22000" {
		t.Fatalf("bad modern forward %q %v", port, err)
	}
}
func TestNativeADBCompatibility(t *testing.T) {
	adb, err := exec.LookPath("adb")
	if err != nil {
		t.Skip("adb binary not installed")
	}
	g, _ := gatewayFixture(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("TCP listeners unavailable: %v", err)
	}
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	go g.Serve(ctx, listener, scopeA())
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	run := func(args ...string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, adb, append([]string{"-H", host, "-P", port}, args...)...)
		out, e := cmd.CombinedOutput()
		if e != nil {
			t.Fatalf("adb %v: %v\n%s", args, e, out)
		}
		return string(out)
	}
	if out := run("devices", "-l"); !strings.Contains(out, "A") || strings.Contains(out, "C\t") {
		t.Fatalf("bad native list: %s", out)
	}
	if out := run("-s", "A", "get-state"); strings.TrimSpace(out) != "device" {
		t.Fatalf("bad state %q", out)
	}
	if out := run("-s", "A", "exec-out", "echo", "hello"); out != "fixture-output\n" {
		t.Fatalf("bad exec %q", out)
	}
	portOut := strings.TrimSpace(run("-s", "A", "forward", "tcp:0", "tcp:80"))
	n, e := strconv.Atoi(portOut)
	if e != nil || n < 1 || n > 65535 {
		t.Fatalf("invalid allocated forward %q", portOut)
	}
	if out := run("forward", "--list"); !strings.Contains(out, "tcp:"+portOut) {
		t.Fatalf("bad forward list %q", out)
	}
	run("-s", "A", "forward", "--remove", "tcp:"+portOut)
	reversePort := strings.TrimSpace(run("-s", "A", "reverse", "tcp:0", "tcp:8080"))
	if _, err := strconv.Atoi(reversePort); err != nil {
		t.Fatalf("bad reverse port %q", reversePort)
	}
	if out := run("-s", "A", "reverse", "--list"); !strings.Contains(out, "tcp:"+reversePort+" tcp:8080") {
		t.Fatalf("bad reverse list %q", out)
	}
	run("-s", "A", "reverse", "--remove-all")
}
