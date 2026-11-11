package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/daemon"
	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
	"github.com/WaferJay/adbgroup/internal/tunnel"
)

func TestMain(m *testing.M) {
	if os.Getenv("ADBGROUP_TEST_CLI") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestManagedTunnelCLIWithMTLS(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "adbgroup-cli-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	files, err := pki.Ensure(filepath.Join(home, "pki"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	user, err := pki.IssueClient(files.CA, files.CAKey, "spiffe://adbgroup/alice", filepath.Join(home, "alice"), 1)
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
	cfg := gateway.Config{Groups: []gateway.Group{{Name: "a", Devices: []string{"A"}}, {Name: "b", Devices: []string{"B"}}}, Identities: map[string][]string{"spiffe://adbgroup/alice": {"a", "b"}}}
	cfg.Defaults()
	e := gateway.NewEngine(cfg)
	g := gateway.NewGateway(e, gateway.Backend{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	remote := tunnel.NewServer(ctx, g)
	s := httptest.NewUnstartedServer((gateway.API{Engine: e, Tunnels: remote.Handler()}).Handler())
	s.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	s.StartTLS()
	defer s.Close()
	defer remote.Close()
	defer g.Forwards.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	invoke := func(args ...string) (string, error) {
		commandCtx, done := context.WithTimeout(context.Background(), 15*time.Second)
		defer done()
		cmd := exec.CommandContext(commandCtx, executable, args...)
		cmd.Env = append(os.Environ(), "ADBGROUP_TEST_CLI=1", "HOME="+home)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	defer invoke("daemon", "stop")
	port := func() string {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer l.Close()
		return l.Addr().String()
	}
	firstPort, secondPort := port(), port()
	create := func(group, address string) string {
		out, err := invoke("t", "-s", s.URL, "-d", filepath.Dir(user.Certificate), "-g", group, "-l", address)
		if err != nil {
			t.Fatalf("create: %s, %v", out, err)
		}
		if !strings.Contains(out, "ADB_SERVER_SOCKET=tcp:"+address) {
			t.Fatal(out)
		}
		conn, err := net.DialTimeout("tcp", address, time.Second)
		if err != nil {
			t.Fatalf("CLI returned before ready: %v", err)
		}
		conn.Close()
		return out
	}
	first := create("a", firstPort)
	create("b", secondPort)
	if reused := create("a", firstPort); reused != first {
		t.Fatalf("duplicate create changed tunnel: %s / %s", first, reused)
	}
	list := func() []daemon.Tunnel {
		out, err := invoke("t", "list", "--json")
		if err != nil {
			t.Fatalf("list: %s, %v", out, err)
		}
		var entries []daemon.Tunnel
		if err := json.Unmarshal([]byte(out), &entries); err != nil {
			t.Fatal(err)
		}
		return entries
	}
	entries := list()
	if len(entries) != 2 {
		t.Fatalf("entries: %+v", entries)
	}
	var firstID string
	for _, info := range entries {
		if info.Listen == firstPort {
			firstID = info.ID
		}
	}
	if out, err := invoke("t", "stop", firstID); err != nil {
		t.Fatalf("stop: %s, %v", out, err)
	}
	if conn, err := net.DialTimeout("tcp", firstPort, time.Second); err == nil {
		conn.Close()
		t.Fatal("stopped port remains open")
	}
	if entries := list(); len(entries) != 1 || entries[0].Listen != secondPort || entries[0].State != "active" {
		t.Fatalf("independent stop: %+v", entries)
	}
	if out, err := invoke("t", "-s", s.URL, "-d", filepath.Dir(user.Certificate), "-g", "a", "-l", secondPort); err == nil {
		t.Fatalf("conflict accepted: %s", out)
	}
	if out, err := invoke("daemon", "stop"); err != nil {
		t.Fatalf("shutdown: %s, %v", out, err)
	}
	if conn, err := net.DialTimeout("tcp", secondPort, time.Second); err == nil {
		conn.Close()
		t.Fatal("daemon stop left port open")
	}
	if out, err := invoke("daemon", "status"); err != nil || !strings.Contains(out, "stopped") {
		t.Fatalf("status: %s, %v", out, err)
	}
	if out, err := invoke("t", "--no-daemon", "--help"); err != nil || !strings.Contains(out, "no-daemon") {
		t.Fatalf("foreground flag: %s, %v", out, err)
	}
	foregroundCtx, foregroundCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer foregroundCancel()
	foreground := exec.CommandContext(foregroundCtx, executable, "t", "--no-daemon", "-s", s.URL, "-d", filepath.Dir(user.Certificate), "-g", "a", "-l", firstPort)
	foreground.Env = append(os.Environ(), "ADBGROUP_TEST_CLI=1", "HOME="+home)
	var foregroundOutput bytes.Buffer
	foreground.Stdout, foreground.Stderr = &foregroundOutput, &foregroundOutput
	if err := foreground.Start(); err != nil {
		t.Fatal(err)
	}
	foregroundDone := make(chan error, 1)
	go func() { foregroundDone <- foreground.Wait() }()
	for {
		conn, err := net.DialTimeout("tcp", firstPort, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		select {
		case err := <-foregroundDone:
			t.Fatalf("foreground exited before ready: %v: %s", err, foregroundOutput.String())
		case <-foregroundCtx.Done():
			t.Fatal("foreground tunnel did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if out, err := invoke("daemon", "status"); err != nil || !strings.Contains(out, "stopped") {
		t.Fatalf("foreground started daemon: %s, %v", out, err)
	}
	if err := foreground.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := <-foregroundDone; err != nil {
		t.Fatalf("foreground shutdown: %v: %s", err, foregroundOutput.String())
	}
	if conn, err := net.DialTimeout("tcp", firstPort, time.Second); err == nil {
		conn.Close()
		t.Fatal("foreground shutdown left port open")
	}
	if out, err := invoke("daemon", "start"); err != nil {
		t.Fatalf("restart: %s, %v", out, err)
	}
	if entries := list(); len(entries) != 0 {
		t.Fatalf("restart restored tunnels: %+v", entries)
	}
}
