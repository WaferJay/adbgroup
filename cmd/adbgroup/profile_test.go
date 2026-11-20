package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
	"github.com/WaferJay/adbgroup/internal/tunnel"
)

func TestProfilesAcrossRenewalAndReconnect(t *testing.T) {
	home, err := os.MkdirTemp("/tmp", "adg-profiles-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	t.Setenv("HOME", home)
	t.Setenv("ADBGROUP_TEST_CLI", "1")
	t.Setenv("ADBGROUP_CONTEXT", "")
	clientCA, err := pki.Ensure(filepath.Join(home, "client-ca"), []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	serverCA, err := pki.Ensure(filepath.Join(home, "server-ca"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(serverCA.Certificate, serverCA.Key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(mustRead(t, clientCA.CA))
	cfg := gateway.Config{API: gateway.APIConfig{Certificate: serverCA.Certificate}, Groups: []gateway.Group{{Name: "team-a"}}, Identities: map[string][]string{"spiffe://adbgroup/alice": {"team-a"}, "spiffe://adbgroup/bob": {"team-a"}}}
	cfg.Defaults()
	g := gateway.NewGateway(gateway.NewEngine(cfg), gateway.Backend{})
	defer g.Forwards.Close()
	remote := tunnel.NewServer(context.Background(), g)
	defer remote.Close()
	api := (gateway.API{Engine: g.Engine, Tunnels: remote.Handler()}).Handler()
	type connKey struct{}
	type connection struct {
		serial string
		conn   net.Conn
	}
	connections := make(chan connection, 16)
	queries := make(chan string, 16)
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serial := r.TLS.PeerCertificates[0].SerialNumber.String()
		if strings.HasSuffix(r.URL.Path, "/control") {
			connections <- connection{serial, r.Context().Value(connKey{}).(net.Conn)}
		}
		if r.URL.Path == "/v1/groups" {
			queries <- serial
		}
		api.ServeHTTP(w, r)
	}))
	s.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context { return context.WithValue(ctx, connKey{}, c) }
	s.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert}
	s.StartTLS()
	defer s.Close()
	defer runDaemonCommand([]string{"stop"}, io.Discard)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(home, "adb"), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", home+":"+os.Getenv("PATH"))
	issue := func(dir, user string) (string, string) {
		t.Helper()
		out := filepath.Join(home, dir)
		if err := issueClientPack(cfg, clientCA.CA, clientCA.CAKey, "spiffe://adbgroup/"+user, out, 1, s.URL, serverCA.CA); err != nil {
			t.Fatal(err)
		}
		chain, err := certificates(mustRead(t, filepath.Join(out, "client.pem")))
		if err != nil {
			t.Fatal(err)
		}
		return filepath.Join(out, "client.adgpack"), chain[0].SerialNumber.String()
	}
	lab, alice := issue("lab", "alice")
	prod, bob := issue("prod", "bob")
	renewed, newAlice := issue("renewal", "alice")
	profileCmd := func(args ...string) {
		t.Helper()
		if err := runProfile(args, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	profileCmd("import", lab)
	profileCmd("import", prod)
	selectGroup := func(name string) []string {
		t.Helper()
		path := filepath.Join(home, "state-"+name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv(shellStateFile, path)
		if err := runUse([]string{"-p", name, "team-a"}, io.Discard); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(mustRead(t, path)), "\n")
		if len(lines) < 4 {
			t.Fatal(lines)
		}
		return lines
	}
	next := func(want string) net.Conn {
		t.Helper()
		select {
		case c := <-connections:
			if c.serial != want {
				t.Fatalf("control connection used %s, want %s", c.serial, want)
			}
			return c.conn
		case <-time.After(12 * time.Second):
			t.Fatal("missing control connection")
			return nil
		}
	}
	a := selectGroup("lab")
	oldConn := next(alice)
	b := selectGroup("prod")
	prodConn := next(bob)
	if a[1] == b[1] {
		t.Fatal("profiles shared a tunnel")
	}
	profileCmd("set-default", "prod")
	profileCmd("update", "lab", renewed)
	checkQuery := func(state []string, want string, flags ...string) {
		t.Helper()
		t.Setenv("ADB_SERVER_SOCKET", state[1])
		t.Setenv("ADBGROUP_CONTEXT", state[2])
		if err := runClientCommand("groups", flags); err != nil {
			t.Fatal(err)
		}
		if got := <-queries; got != want {
			t.Fatalf("query used %s, want %s", got, want)
		}
	}
	checkQuery(a, alice)
	checkQuery(b, bob)
	checkQuery(a, newAlice, "-p", "lab")
	if output, err := exec.Command(executable, "-p", "lab", "team-a", "devices").CombinedOutput(); err != nil {
		t.Fatalf("one-off adb: %s %v", output, err)
	}
	next(newAlice)
	oldConn.Close()
	next(alice)
	prodConn.Close()
	next(bob)
}

func TestPackAndProfileImport(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	ca, err := pki.Ensure(filepath.Join(home, "issuer"), []string{"first.example", "127.0.0.1", "*.test.example"})
	if err != nil {
		t.Fatal(err)
	}
	cfg := gateway.Config{API: gateway.APIConfig{Listen: ":8443", Certificate: ca.Certificate}, PKI: gateway.PKIConfig{ServerNames: []string{"unissued.example", "first.example", "127.0.0.1"}}}
	servers, err := packServers(cfg, "")
	if err != nil || !slices.Equal(servers, []string{"https://first.example:8443", "https://127.0.0.1:8443"}) {
		t.Fatalf("candidate list %v: %v", servers, err)
	}
	cfg.PKI.ServerNames = nil
	servers, err = packServers(cfg, "")
	if err != nil || len(servers) != 2 {
		t.Fatalf("SAN fallback %v: %v", servers, err)
	}
	target := "https://some.test.example:9443"
	dir := filepath.Join(home, "lab")
	if err := issueClientPack(cfg, ca.CA, ca.CAKey, "spiffe://adbgroup/test", dir, 1, "https://wrong.example", ""); err == nil {
		t.Fatal("wrong hostname accepted")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("published incomplete credentials", err)
	}
	if err := issueClientPack(cfg, ca.CA, ca.CAKey, "spiffe://adbgroup/test", dir, 1, target, ""); err != nil {
		t.Fatal(err)
	}
	pack := filepath.Join(dir, "client.adgpack")
	repack := filepath.Join(home, "second.adgpack")
	if err := writeClientPack(cfg, dir, repack, target, ""); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{pack, repack} {
		info, err := os.Stat(file)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("pack permissions", err)
		}
		m, c, err := readClientPack(file)
		if err != nil {
			t.Fatal(err)
		}
		if m.Name != "lab" || !slices.Equal(m.Servers, []string{target}) {
			t.Fatal(m)
		}
		for name, data := range map[string][]byte{"ca.pem": c.ca, "client.pem": c.cert, "client.key": c.key} {
			if !bytes.Equal(data, mustRead(t, filepath.Join(dir, name))) {
				t.Fatal(name)
			}
		}
	}
	path, _ := defaultTunnelConfig()
	root := filepath.Join(filepath.Dir(path), "profiles")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(fmt.Sprintf("server: %s\ncredentials: %q\nlisten: 18888\n", target, dir)), 0600); err != nil {
		t.Fatal(err)
	}
	// Concurrent imports of the same credentials and a legacy YAML migration.
	done := make(chan error, 3)
	for _, args := range [][]string{{"import", pack}, {"import", repack}, {"import", "-f", path, "-name", "legacy"}} {
		go func() { done <- runProfile(args, io.Discard) }()
	}
	for range 3 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	index, err := readProfiles(root)
	if err != nil || len(index.Profiles) != 2 || index.Default != "" || index.Profiles["legacy"].Listen != "18888" {
		t.Fatalf("%+v %v", index, err)
	}
	if err := runProfile([]string{"set-default", "legacy"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	original := mustRead(t, filepath.Join(root, "profiles.json"))
	if err := runProfile([]string{"import", "-server", "https://other.example", pack}, io.Discard); err == nil {
		t.Fatal("overwrote conflicting profile")
	}
	tampered := filepath.Join(home, "bad.zip")
	var buf bytes.Buffer
	z := zip.NewWriter(&buf)
	w, _ := z.Create("../client.key")
	fmt.Fprint(w, "test")
	z.Close()
	if err := os.WriteFile(tampered, buf.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := runProfile([]string{"update", "legacy", tampered}, io.Discard); err == nil {
		t.Fatal("accepted unexpected archive entry")
	}
	if !bytes.Equal(original, mustRead(t, filepath.Join(root, "profiles.json"))) {
		t.Fatal("failed import changed profiles")
	}
	other, err := pki.Ensure(filepath.Join(home, "other-ca"), []string{"localhost"})
	if err != nil {
		t.Fatal(err)
	}
	rotated := filepath.Join(home, "rotated.adgpack")
	if err := writeClientPack(cfg, dir, rotated, target, other.CA); err != nil {
		t.Fatal(err)
	}
	input, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	writer.Close()
	defer input.Close()
	stdin := os.Stdin
	os.Stdin = input
	defer func() { os.Stdin = stdin }()
	var output bytes.Buffer
	err = runProfile([]string{"update", "legacy", rotated}, &output)
	if err == nil || !strings.Contains(output.String(), "New:") || !bytes.Equal(original, mustRead(t, filepath.Join(root, "profiles.json"))) {
		t.Fatal("unconfirmed CA change", err, output.String())
	}
	flags := newCommandFlags("groups")
	opts := bindClientFlags(flags)
	flags.String("f", "", "config")
	if err := flags.Parse([]string{"-p", "lab", "-f", path}); err != nil {
		t.Fatal(err)
	}
	if err := opts.loadConfig(path, flags); err == nil {
		t.Fatal("accepted profile and config together")
	}
	var check profileIndex
	if err := json.Unmarshal(original, &check); err != nil || check.Default != "legacy" {
		t.Fatal(check, err)
	}
}
