package main

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestServeProvisionsConfiguredProfiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	config := filepath.Join(home, "config.yaml")
	configText := "api:\n  listen: 127.0.0.1:0\npki:\n  server_names: [127.0.0.1]\nidentities:\n  'spiffe://adbgroup/alice': ['*']\n"
	start := func(checkReady func() error) {
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
		var checkErr error
		for scanner.Scan() {
			lines = append(lines, scanner.Text())
			if strings.Contains(scanner.Text(), "adg ready") {
				ready = true
				if checkReady != nil {
					checkErr = checkReady()
				}
				_ = cmd.Process.Signal(syscall.SIGTERM)
			}
		}
		if err := cmd.Wait(); err != nil || !ready {
			t.Fatalf("serve: %v, logs %v", err, lines)
		}
		if checkErr != nil {
			t.Fatalf("client check: %v", checkErr)
		}
	}
	// Reserve a different port for the second startup while the first binds :0.
	nextListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer nextListener.Close()
	nextAddr := nextListener.Addr().String()
	start(nil)
	root := filepath.Join(home, "run", "pki", "clients")
	pack := filepath.Join(root, "alice", "client.adgpack")
	key := filepath.Join(root, "alice", "client.key")
	original, before, err := readClientPack(pack)
	if err != nil || len(original.Servers) != 1 {
		t.Fatalf("initial pack should contain the bound port: %v, %v", original.Servers, err)
	}
	if err := nextListener.Close(); err != nil {
		t.Fatal(err)
	}
	configText = strings.Replace(configText, "127.0.0.1:0", nextAddr, 1)
	configText += "  'spiffe://example/team/bob': ['*']\n"
	start(func() error {
		if err := runProfile([]string{"import", pack}, io.Discard); err != nil {
			return err
		}
		return runClientCommand("groups", []string{"-p", "alice"})
	})
	updated, after, err := readClientPack(pack)
	expected := "https://" + nextAddr
	if err != nil || !slices.Equal(updated.Servers, []string{expected}) || slices.Equal(original.Servers, updated.Servers) {
		t.Fatalf("expected updated server %s, got %v (previously %v): %v", expected, updated.Servers, original.Servers, err)
	}
	if updated.Name != original.Name || !bytes.Equal(before.cert, after.cert) || !bytes.Equal(before.key, after.key) || !bytes.Equal(before.ca, after.ca) {
		t.Fatal("repacking changed the profile name or credentials")
	}
	updatedFile, err := os.Stat(pack)
	if err != nil {
		t.Fatal(err)
	}
	start(nil)
	if st, err := os.Stat(pack); err != nil || !os.SameFile(updatedFile, st) {
		t.Fatal("restart with the same endpoint replaced the pack")
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
	start(nil)
	_, c, err = readClientPack(pack)
	if err != nil || !bytes.Equal(c.key, before.key) {
		t.Fatal("missing pack was not restored from existing key", err)
	}
	cfg, err := load(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(key); err != nil {
		t.Fatal(err)
	}
	if err := ensureStartupPacks(cfg, nextAddr); err == nil {
		t.Fatal("accepted partial export")
	}
	cfg.PKI.AutoTLS = false
	cfg.PKI.Directory = filepath.Join(home, "external")
	if err := ensureStartupPacks(cfg, nextAddr); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.PKI.Directory); !os.IsNotExist(err) {
		t.Fatal("external PKI was modified", err)
	}
}
