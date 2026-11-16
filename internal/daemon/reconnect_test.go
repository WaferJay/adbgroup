package daemon

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
	"github.com/WaferJay/adbgroup/internal/tunnel"
)

type restartServer struct {
	config Config
	files  pki.Files
	tls    *tls.Config
	status atomic.Int32
	engine *gateway.Engine
	stop   func()
}

func newRestartServer(t *testing.T) *restartServer {
	t.Helper()
	dir := t.TempDir()
	files, err := pki.Ensure(filepath.Join(dir, "pki"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	user, err := pki.IssueClient(files.CA, files.CAKey, "spiffe://adbgroup/alice", filepath.Join(dir, "alice"), 1)
	if err != nil {
		t.Fatal(err)
	}
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
	s := &restartServer{files: files, tls: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}}
	s.config = Config{Group: "a", Cert: user.Certificate, Key: user.Key, CA: user.CA}
	s.start(t)
	t.Cleanup(func() { s.stop() })
	return s
}

func (s *restartServer) start(t *testing.T) {
	t.Helper()
	address := strings.TrimPrefix(s.config.Server, "https://")
	if address == "" {
		address = "127.0.0.1:0"
	}
	l, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	s.config.Server = "https://" + l.Addr().String()
	cfg := gateway.Config{Groups: []gateway.Group{{Name: "a", Devices: []string{"A"}}}, Identities: map[string][]string{"spiffe://adbgroup/alice": {"a"}}}
	cfg.Defaults()
	e := gateway.NewEngine(cfg)
	s.engine = e
	e.Update([]gateway.Device{{Serial: "A", State: "device", TransportID: "1"}})
	g := gateway.NewGateway(e, gateway.Backend{Dial: func(ctx context.Context) (net.Conn, error) {
		a, b := net.Pipe()
		go func() {
			defer b.Close()
			for {
				req, err := gateway.ReadFrame(b)
				if err != nil {
					return
				}
				io.WriteString(b, "OKAY")
				if strings.HasPrefix(req, "host:transport-id:") {
					continue
				}
				io.Copy(b, b)
				return
			}
		}()
		return a, nil
	}})
	ctx, cancel := context.WithCancel(context.Background())
	remote := tunnel.NewServer(ctx, g)
	api := (gateway.API{Engine: e, Tunnels: remote.Handler()}).Handler()
	h := &http.Server{TLSConfig: s.tls, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status := int(s.status.Load()); status != 0 && r.Method == "POST" {
			http.Error(w, "test rejection", status)
			return
		}
		api.ServeHTTP(w, r)
	})}
	go h.ServeTLS(l, "", "")
	go e.Run(ctx)
	var once sync.Once
	s.stop = func() { once.Do(func() { cancel(); remote.Close(); h.Close(); g.Forwards.Close() }) }
}

func localADB(t *testing.T, address string) net.Conn {
	t.Helper()
	c, err := net.DialTimeout("tcp", address, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	c.SetDeadline(time.Now().Add(5 * time.Second))
	t.Cleanup(func() { c.Close() })
	return c
}

func expectStatus(t *testing.T, c net.Conn, request, expected string) {
	t.Helper()
	if err := gateway.WriteFrame(c, request); err != nil {
		t.Fatal(err)
	}
	var status [4]byte
	if _, err := io.ReadFull(c, status[:]); err != nil {
		t.Fatal(err)
	}
	if string(status[:]) != expected {
		t.Fatalf("status %q, want %q", status, expected)
	}
}

func TestManagedTunnelServerRestartRetainsPortAndIdentity(t *testing.T) {
	s := newRestartServer(t)
	m := NewManager()
	defer m.Close()
	// Resolve the group once; reconnection must keep the resolved group.
	cfg := s.config
	cfg.Group = ""
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen = l.Addr().String()
	l.Close()
	first, err := m.Start(testContext(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if reused, err := m.Start(testContext(t), cfg); err != nil || reused.ID != first.ID {
		t.Fatalf("inferred group reuse: %+v %v", reused, err)
	}
	stream := localADB(t, first.Listen)
	expectStatus(t, stream, "host:transport:A", "OKAY")
	expectStatus(t, stream, "tcp:1234", "OKAY")
	if _, err := io.WriteString(stream, "before"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 6)
	if _, err := io.ReadFull(stream, buf); err != nil || string(buf) != "before" {
		t.Fatalf("echo: %q %v", buf, err)
	}
	s.stop()
	eventually(t, func() bool { return m.List()[0].State == "reconnecting" })
	if _, err := stream.Read(buf); err == nil {
		t.Fatal("old stream survived restart")
	}
	if l, err := net.Listen("tcp", first.Listen); err == nil {
		l.Close()
		t.Fatal("local port released during reconnect")
	}
	offline := localADB(t, first.Listen)
	expectStatus(t, offline, "host:devices", "FAIL")
	message, err := gateway.ReadFrame(offline)
	if err != nil || !strings.Contains(message, "unavailable") {
		t.Fatalf("offline: %q %v", message, err)
	}
	// Cancelling a wait for an existing tunnel must leave its retry loop alive.
	waitCtx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := m.Ensure(waitCtx, s.config); err == nil {
		t.Fatal("offline ensure unexpectedly succeeded")
	}
	if m.List()[0].State != "reconnecting" {
		t.Fatal("cancelled wait stopped reconnect")
	}
	s.start(t)
	recovered, err := m.Ensure(testContext(t), s.config)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.ID != first.ID || recovered.Listen != first.Listen || recovered.Group != "a" || recovered.Error != "" {
		t.Fatalf("changed binding: %+v -> %+v", first, recovered)
	}
	c := localADB(t, first.Listen)
	expectStatus(t, c, "host:devices", "OKAY")
	devices, err := gateway.ReadFrame(c)
	if err != nil || !strings.Contains(devices, "A\tdevice") {
		t.Fatalf("devices: %q %v", devices, err)
	}
	if err := m.Stop(testContext(t), first.ID); err != nil {
		t.Fatal(err)
	}
	if c, err := net.DialTimeout("tcp", first.Listen, time.Second); err == nil {
		c.Close()
		t.Fatal("stop retained listener")
	}
}

func TestManagedTunnelMissingGroupRejectionAndExplicitRetry(t *testing.T) {
	s := newRestartServer(t)
	m := NewManager()
	defer m.Close()
	first, err := m.Ensure(testContext(t), s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.stop()
	s.status.Store(404)
	s.start(t)
	eventually(t, func() bool { return strings.Contains(m.List()[0].Error, "404") })
	if m.List()[0].State != "reconnecting" {
		t.Fatal("missing group stopped retrying")
	}
	s.status.Store(403)
	deadline := time.Now().Add(6 * time.Second)
	for m.List()[0].State != "failed" {
		if time.Now().After(deadline) {
			t.Fatal("403 did not stop reconnecting")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(m.List()[0].Error, "403") {
		t.Fatal(m.List())
	}
	c := localADB(t, first.Listen)
	expectStatus(t, c, "host:devices", "FAIL")
	// Invalid replacement credentials keep the original listener reserved too.
	original, err := os.ReadFile(s.config.Cert)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.config.Cert, []byte("invalid"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(testContext(t), s.config); err == nil {
		t.Fatal("invalid certificate accepted")
	}
	if err := os.WriteFile(s.config.Cert, original, 0600); err != nil {
		t.Fatal(err)
	}
	s.status.Store(0)
	retried, err := m.Ensure(testContext(t), s.config)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID != first.ID || retried.Listen != first.Listen {
		t.Fatal(fmt.Sprint(first, retried))
	}
}

func TestManagedInitialFailureRequiresRetry(t *testing.T) {
	s := newRestartServer(t)
	s.status.Store(503)
	m := NewManager()
	defer m.Close()
	first, err := m.Ensure(testContext(t), s.config)
	if err == nil || first.State != "failed" || first.Listen != "" {
		t.Fatalf("initial failure: %+v %v", first, err)
	}
	s.status.Store(0)
	if m.List()[0].State != "failed" {
		t.Fatal("initial failure began retrying")
	}
	retried, err := m.Ensure(testContext(t), s.config)
	if err != nil || retried.ID != first.ID || retried.State != "active" {
		t.Fatalf("explicit retry after initial failure: %+v %v", retried, err)
	}
}

func TestManagedWriteLeaseDoesNotReconnect(t *testing.T) {
	s := newRestartServer(t)
	lease, err := s.engine.CreateLease("spiffe://adbgroup/alice", "a", "A", time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	m := NewManager()
	defer m.Close()
	cfg := s.config
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Listen, cfg.Lease = l.Addr().String(), lease.ID
	l.Close()
	first, err := m.Start(testContext(t), cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.stop()
	eventually(t, func() bool { return m.List()[0].State == "failed" })
	l, err = net.Listen("tcp", first.Listen)
	if err != nil {
		t.Fatalf("write tunnel kept local port: %v", err)
	}
	l.Close()
}
