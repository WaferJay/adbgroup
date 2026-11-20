package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

func TestReconnectErrorClassificationAndBackoff(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 408, 409, 429, 500, 503} {
		want := status == 404 || status == 408 || status == 429 || status >= 500
		if retryable(fmt.Errorf("wrapped: %w", &HTTPError{Status: status})) != want {
			t.Fatalf("HTTP %d", status)
		}
	}
	for _, err := range []error{permanentError{errors.New("protocol")}, x509.UnknownAuthorityError{}, x509.CertificateInvalidError{}, x509.HostnameError{}, &tls.CertificateVerificationError{Err: errors.New("invalid")}, &net.OpError{Op: "remote error", Err: errors.New("tls alert")}} {
		if retryable(fmt.Errorf("wrapped: %w", err)) {
			t.Fatalf("retried terminal error %T", err)
		}
	}
	if !retryable(context.DeadlineExceeded) {
		t.Fatal("timeout is not retryable")
	}
	for attempt := 0; attempt < 10; attempt++ {
		base := min(time.Second<<min(attempt, 5), 30*time.Second)
		for i := 0; i < 20; i++ {
			d := reconnectDelay(attempt)
			if d < base*8/10 || d > base*12/10 {
				t.Fatalf("attempt %d: %s", attempt, d)
			}
		}
	}
}

func TestEndpointReconnectClearsRulesAndStopDuringBackoff(t *testing.T) {
	h := newHarness(t)
	dir := filepath.Join(filepath.Dir(filepath.Dir(h.issuer.CA)), "alice")
	endpoint := NewEndpoint("127.0.0.1:15037", func() (*Client, error) {
		c, err := NewClient("https://adb.test:8443", filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.pem"))
		if err == nil {
			c.http.Transport.(*http.Transport).DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) { return h.host.dial(ctx, "127.0.0.1:8443") }
			c.listen, c.dial = h.local.listen, h.local.dial
		}
		return c, err
	})
	endpoint.listen = h.local.listen
	delays := make(chan int, 4)
	endpoint.backoff = func(attempt int) time.Duration {
		delays <- attempt
		if attempt == 0 {
			return time.Millisecond
		}
		return time.Hour
	}
	ready := make(chan string, 4)
	done := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer func() { cancel(); endpoint.Close(); <-done }()
	go func() {
		done <- endpoint.Run(ctx, "a", func(group, address string) { ready <- group + " " + address }, func(string, error) {})
	}()
	select {
	case resolved := <-ready:
		if resolved != "a 127.0.0.1:15037" {
			t.Fatal(resolved)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("not ready")
	}
	c := h.adb(t, "host:forward:tcp:0;tcp:80")
	status(t, c)
	port, err := gateway.ReadFrame(c)
	c.Close()
	if err != nil {
		t.Fatal(err)
	}
	c = h.adb(t, "reverse:forward:tcp:8080;tcp:18080")
	status(t, c)
	c.Close()
	closeSession := func() {
		h.server.mu.Lock()
		var current *session
		for _, s := range h.server.sessions {
			s.mu.Lock()
			active := s.State == "active"
			s.mu.Unlock()
			if active {
				current = s
				break
			}
		}
		h.server.mu.Unlock()
		if current == nil {
			t.Fatal("no active remote session")
		}
		current.close()
		<-current.cleaned
	}
	closeSession()
	select {
	case <-ready:
	case <-time.After(3 * time.Second):
		t.Fatal("did not reconnect")
	}
	if attempt := <-delays; attempt != 0 {
		t.Fatal(attempt)
	}
	h.local.mu.Lock()
	forwardExists := h.local.listeners["127.0.0.1:"+port] != nil
	h.local.mu.Unlock()
	h.backend.mu.Lock()
	reverseCount := len(h.backend.reverse)
	h.backend.mu.Unlock()
	if forwardExists || reverseCount != 0 {
		t.Fatal("rules survived reconnection")
	}
	c = h.adb(t, "tcp:1234")
	echo(t, c)
	c.Close()
	closeSession()
	select {
	case attempt := <-delays:
		if attempt != 1 {
			t.Fatalf("flapping reset backoff: %d", attempt)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("not backing off")
	}
	// Stop must cancel a long backoff and close idle local connections too.
	idle, err := h.local.dial(ctx, "127.0.0.1:15037")
	if err != nil {
		t.Fatal(err)
	}
	defer idle.Close()
	cancel()
	endpoint.Close()
	if _, err := h.local.dial(context.Background(), "127.0.0.1:15037"); err == nil {
		t.Fatal("listener survived stop")
	}
}

func TestEndpointInitialFailureDoesNotRetry(t *testing.T) {
	attempts := 0
	e := NewEndpoint("", func() (*Client, error) { attempts++; return nil, errors.New("invalid credentials") })
	defer e.Close()
	if err := e.Run(context.Background(), "a", func(string, string) { t.Fatal("ready") }, func(string, error) { t.Fatal("retry") }); err == nil || attempts != 1 {
		t.Fatalf("initial failure: %v (%d attempts)", err, attempts)
	}
}
