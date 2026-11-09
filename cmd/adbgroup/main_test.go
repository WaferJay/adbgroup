package main

import (
	"adbgroup/internal/pki"
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
