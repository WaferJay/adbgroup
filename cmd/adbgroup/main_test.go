package main

import (
	"adbgroup/internal/pki"
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTunnelShortCommandAndDefaults(t *testing.T) {
	if got := canonicalCommand("t"); got != "tunnel" {
		t.Fatalf("canonicalCommand(t) = %q", got)
	}
	flags := flag.NewFlagSet("t", flag.ContinueOnError)
	o := bindTunnelFlags(flags)
	if err := flags.Parse([]string{"-s", "https://adb.example", "-d", "alice", "-g", "team-a", "-l", "127.0.0.1:15038", "-w", "lease-id"}); err != nil {
		t.Fatal(err)
	}
	cert, key, ca := o.credentialFiles()
	if o.server != "https://adb.example" || o.group != "team-a" || o.listen != "127.0.0.1:15038" || o.lease != "lease-id" {
		t.Fatalf("short flags not applied: %+v", o)
	}
	if cert != filepath.Join("alice", "client.pem") || key != filepath.Join("alice", "client.key") || ca != filepath.Join("alice", "ca.pem") {
		t.Fatalf("credential defaults = %q, %q, %q", cert, key, ca)
	}
	o.applyFile(tunnelFile{Server: "https://ignored.example", Group: "from-config", Credentials: "/config/credentials", Certificate: "/config/client.pem"}, flags)
	if o.server != "https://adb.example" || o.group != "team-a" || o.credentials != "alice" || o.cert != "" {
		t.Fatalf("config overrode command line: %+v", o)
	}

	flags = flag.NewFlagSet("t", flag.ContinueOnError)
	o = bindTunnelFlags(flags)
	cert, key, ca = o.credentialFiles()
	if o.server != defaultTunnelServer || o.group != "" || o.listen != "127.0.0.1:15037" || cert != "client.pem" || key != "client.key" || ca != "ca.pem" {
		t.Fatalf("unexpected tunnel defaults: %+v, %q, %q, %q", o, cert, key, ca)
	}
}

func TestTunnelConfigDefaultPathAndRelativeCredentials(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path, err := defaultTunnelConfig()
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(home, ".config", "adbgroup", "tunnel.yaml")
	if path != wantPath {
		t.Fatalf("default config path = %q, want %q", path, wantPath)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("server: https://adb.example\ngroup: team-a\ncredentials: credentials\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c, found, err := loadTunnelFile(path)
	if err != nil || !found {
		t.Fatalf("load tunnel config: found=%v error=%v", found, err)
	}
	if c.Server != "https://adb.example" || c.Group != "team-a" || c.Credentials != filepath.Join(filepath.Dir(path), "credentials") {
		t.Fatalf("tunnel config = %+v", c)
	}
	if err = os.WriteFile(path, []byte("unknown: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = loadTunnelFile(path); err == nil {
		t.Fatal("unknown tunnel config field accepted")
	}
}

func TestLoadExampleAndRejectUnknownOrExtraDocuments(t *testing.T) {
	example, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err = os.WriteFile(path, example, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(c.API.Certificate) || !c.Groups[0].Contains("USB_SERIAL_SHARED") {
		t.Fatal("paths or shared membership not loaded")
	}
	barePort := strings.Replace(string(example), "address: 127.0.0.1:5038", "address: 5038", 1)
	if err = os.WriteFile(path, []byte(barePort), 0600); err != nil {
		t.Fatal(err)
	}
	if c, err = load(path); err != nil || c.Backend.Address != "5038" {
		t.Fatalf("bare backend port not loaded: address=%q error=%v", c.Backend.Address, err)
	}
	for _, s := range []string{string(example) + "\nunknown_field: true\n", string(example) + "\n---\n{}\n", string(example) + "\nlease_ssh: {}\n", strings.Replace(string(example), "name: team-b", "name: team-a", 1)} {
		if err = os.WriteFile(path, []byte(s), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = load(path); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
}

func TestDefaultPKILoadAndCertCommand(t *testing.T) {
	example, err := os.ReadFile("../../config.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, example, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !c.PKI.AutoTLS || c.API.Certificate != filepath.Join(dir, "run", "pki", "server.pem") || c.PKI.Directory != filepath.Join(dir, "run", "pki") {
		t.Fatalf("incorrect generated paths: %+v", c)
	}
	if _, err := os.Stat(c.PKI.Directory); !os.IsNotExist(err) {
		t.Fatal("loading configuration created PKI state")
	}
	previous := os.Args
	defer func() { os.Args = previous }()
	os.Args = []string{"adbgroup", "check", "-config", configPath}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.PKI.Directory); !os.IsNotExist(err) {
		t.Fatal("check command created PKI state")
	}
	out := filepath.Join(dir, "alice")
	os.Args = []string{"adbgroup", "cert", "-config", configPath, "-user", "alice", "-days", "30", "-out", out}
	if err := run(); err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(mustRead(t, filepath.Join(out, "client.pem")))
	if block == nil {
		t.Fatal("no client certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://adbgroup/alice" {
		t.Fatal("wrong CLI identity")
	}
	ca := pki.ManagedFiles(c.PKI.Directory)
	before := mustRead(t, ca.CA)
	if err := run(); err == nil {
		t.Fatal("CLI overwrote existing client export")
	}
	if !bytes.Equal(before, mustRead(t, ca.CA)) {
		t.Fatal("CA replaced by issuance")
	}
	os.Args = []string{"adbgroup", "cert", "-config", configPath, "-user", "bob", "-identity", "spiffe://adbgroup/bob", "-out", filepath.Join(dir, "bad")}
	if err := run(); err == nil {
		t.Fatal("conflicting identity flags accepted")
	}
}

func TestADBServerArgsFollowBackendAddressForm(t *testing.T) {
	for _, tc := range []struct {
		address string
		want    string
	}{
		{"127.0.0.1:5038", "-L tcp:localhost:5038 server nodaemon"},
		{"5038", "-a -L tcp:5038 server nodaemon"},
	} {
		got, err := adbServerArgs(tc.address)
		if err != nil {
			t.Fatal(err)
		}
		if joined := strings.Join(got, " "); joined != tc.want {
			t.Fatalf("adbServerArgs(%q) = %q, want %q", tc.address, joined, tc.want)
		}
	}
	if _, err := adbServerArgs("invalid"); err == nil {
		t.Fatal("invalid backend address accepted")
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
