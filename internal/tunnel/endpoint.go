package tunnel

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

// HTTPError preserves the status needed to distinguish rejection from outage.
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

func retryable(err error) bool {
	var permanent permanentError
	var verification *tls.CertificateVerificationError
	var invalid x509.CertificateInvalidError
	var unknown x509.UnknownAuthorityError
	var hostname x509.HostnameError
	if errors.As(err, &permanent) || errors.As(err, &verification) || errors.As(err, &invalid) || errors.As(err, &unknown) || errors.As(err, &hostname) {
		return false
	}
	var status *HTTPError
	if errors.As(err, &status) {
		return status.Status >= 500 || status.Status == 404 || status.Status == 408 || status.Status == 429
	}
	// TLS alerts can arrive as remote record errors, including expired client certificates.
	var alert tls.AlertError
	var operation *net.OpError
	if errors.As(err, &alert) || errors.As(err, &operation) && operation.Op == "remote error" {
		return false
	}
	return true
}

// Endpoint retains one local ADB listener across authenticated remote sessions.
// Run may be retried after failure; Close permanently releases the local port.
type Endpoint struct {
	mu        sync.Mutex
	address   string
	listener  net.Listener
	client    *Client
	closed    bool
	conns     map[net.Conn]struct{}
	wg        sync.WaitGroup
	newClient func() (*Client, error)
	listen    func(string, string) (net.Listener, error)
	backoff   func(int) time.Duration
	stable    time.Duration
}

func NewEndpoint(address string, factory func() (*Client, error)) *Endpoint {
	if address == "" {
		address = "127.0.0.1:0"
	}
	return &Endpoint{address: address, newClient: factory, listen: net.Listen, conns: map[net.Conn]struct{}{}, backoff: reconnectDelay, stable: 30 * time.Second}
}

func reconnectDelay(attempt int) time.Duration {
	base := time.Second << min(attempt, 5)
	base = min(base, 30*time.Second)
	return time.Duration(float64(base) * (0.8 + 0.4*rand.Float64()))
}

// Run reconnects only after a session has reached ready. Every attempt reloads
// credentials through the factory. A failed endpoint keeps its listener until Close.
func (e *Endpoint) Run(ctx context.Context, group string, ready func(string, string), state func(string, error)) error {
	established := false
	lastError := ""
	attempt := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		c, err := e.newClient()
		if err != nil {
			return err // Invalid local credentials require explicit intervention.
		}
		attemptCtx, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(30*time.Second, cancel)
		var activeSince time.Time
		err = c.runSession(attemptCtx, group, "", func() error {
			if !timer.Stop() || attemptCtx.Err() != nil {
				return context.DeadlineExceeded
			}
			e.mu.Lock()
			if e.closed {
				e.mu.Unlock()
				return net.ErrClosed
			}
			if e.listener == nil {
				e.listener, err = e.listen("tcp", e.address)
				if err != nil {
					e.mu.Unlock()
					return permanentError{err}
				}
				e.address = e.listener.Addr().String()
				e.wg.Add(1)
				go e.accept()
			}
			e.client = c
			address := e.address
			e.mu.Unlock()
			established = true
			activeSince = time.Now()
			ready(group, address)
			slog.Info("managed tunnel active", "group", group, "listen", address)
			lastError = ""
			return nil
		}, func() {
			e.mu.Lock()
			if e.client == c {
				e.client = nil
			}
			e.mu.Unlock()
			if established && ctx.Err() == nil {
				state("reconnecting", errors.New("control disconnected; forward/reverse rules cleared"))
			}
		})
		timer.Stop()
		cancel()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err == nil {
			err = errors.New("remote session ended")
		}
		if !established || !retryable(err) {
			return err
		}
		if !activeSince.IsZero() && time.Since(activeSince) >= e.stable {
			attempt = 0
		}
		state("reconnecting", err)
		if err.Error() != lastError {
			slog.Warn("managed tunnel reconnecting", "group", group, "error", err)
			lastError = err.Error()
		}
		delay := e.backoff(attempt)
		attempt++
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
	}
}

func (e *Endpoint) accept() {
	defer e.wg.Done()
	for {
		raw, err := e.listener.Accept()
		if err != nil {
			return
		}
		e.mu.Lock()
		if e.closed {
			e.mu.Unlock()
			raw.Close()
			return
		}
		c := e.client
		if c != nil && c.track(raw) {
			e.wg.Add(1)
			go func() { defer e.wg.Done(); c.pipeLocal(raw, "adb", "") }()
			e.mu.Unlock()
			continue
		}
		if len(e.conns) >= 128 {
			e.mu.Unlock()
			raw.Close()
			continue
		}
		e.conns[raw] = struct{}{}
		e.wg.Add(1)
		e.mu.Unlock()
		go e.unavailable(raw)
	}
}

func (e *Endpoint) unavailable(raw net.Conn) {
	defer e.wg.Done()
	defer func() { raw.Close(); e.mu.Lock(); delete(e.conns, raw); e.mu.Unlock() }()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := gateway.ReadFrame(raw); err == nil {
		_, _ = io.WriteString(raw, "FAIL")
		_ = gateway.WriteFrame(raw, "adbgroup tunnel unavailable; reconnecting or awaiting explicit retry")
	}
}

func (e *Endpoint) Close() {
	e.mu.Lock()
	e.closed = true
	if e.listener != nil {
		e.listener.Close()
	}
	for c := range e.conns {
		c.Close()
	}
	c := e.client
	e.mu.Unlock()
	if c != nil {
		c.shutdown()
	}
	e.wg.Wait()
}
