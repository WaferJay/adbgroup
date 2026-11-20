package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServeProvisionsConfiguredProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := filepath.Join(home, "config.yaml")
	configText := "api:\n  listen: 127.0.0.1:0\npki:\n  server_names: [localhost]\nidentities:\n  'spiffe://adbgroup/alice': ['*']\n"
	start := func() {
		t.Helper()
		if err := os.WriteFile(config, []byte(configText), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, executable, "serve", "-config", config)
		cmd.Env = append(os.Environ(), "ADBGROUP_TEST_CLI=1")
		stderr, err := cmd.StderrPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = cmd.Start(); err != nil {
			t.Fatal(err)
		}
		scanner := bufio.NewScanner(stderr)
		var lines []string
		ready := false
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
			if strings.Contains(scanner.Text(), "adg ready") {
				ready = true
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		}
		if err := cmd.Wait(); err != nil || !ready {
			t.Fatalf("serve: %v, logs %v", err, lines)
		}
	}
	start()
	root := filepath.Join(home, "run", "pki", "clients")
	pack := filepath.Join(root, "alice", "client.adgpack")
	key := filepath.Join(root, "alice", "client.key")
	original, keyBytes := mustRead(t, pack), mustRead(t, key)
	if err := runProfile([]string{"import", "-server", "https://localhost:8443", pack}, io.Discard); err != nil {
		t.Fatal(err)
	}
	configText += "  'spiffe://example/team/bob': ['*']\n"
	start()
	if !bytes.Equal(original, mustRead(t, pack)) || !bytes.Equal(keyBytes, mustRead(t, key)) {
		t.Fatal("restart replaced existing credentials")
	}
	custom := filepath.Join(root, identityExportName("spiffe://example/team/bob"), "client.adgpack")
	_, c, err := readClientPack(custom)
	if err != nil {
		t.Fatal(err)
	}
	info, err := c.validate()
	if err != nil || info.leaf.URIs[0].String() != "spiffe://example/team/bob" {
		t.Fatal("custom identity", err)
	}
	if err := os.Remove(pack); err != nil {
		t.Fatal(err)
	}
	start()
	_, c, err = readClientPack(pack)
	if err != nil || !bytes.Equal(c.key, keyBytes) {
		t.Fatal("missing pack was not restored from existing key", err)
	}
	cfg, err := load(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := ensureStartupPacks(cfg); err == nil {
		t.Fatal("accepted partial export")
	}
	cfg.PKI.AutoTLS = false
	cfg.PKI.Directory = filepath.Join(home, "external")
	if err := ensureStartupPacks(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.PKI.Directory); !os.IsNotExist(err) {
		t.Fatal("external PKI was modified", err)
	}
}
