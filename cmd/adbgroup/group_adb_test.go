package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/daemon"
	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
	"github.com/WaferJay/adbgroup/internal/tunnel"
)

func TestGroupSelectorBoundary(t *testing.T) {
	for _, tc := range []struct {
		args  []string
		group string
		adb   []string
	}{
		{[]string{"-f", "@profile", "@t", "shell", "echo", "@file"}, "t", []string{"shell", "echo", "@file"}},
		{[]string{"@@serial", "devices", "-l"}, "@serial", []string{"devices", "-l"}},
		{[]string{"-g", "@serial", "devices"}, "@serial", []string{"devices"}},
	} {
		flags := flag.NewFlagSet("adg", flag.ContinueOnError)
		o := bindClientFlags(flags)
		flags.String("f", "", "config")
		group, args, err := parseGroupArgs(flags, o, tc.args)
		if err != nil || group != tc.group || !reflect.DeepEqual(args, tc.adb) {
			t.Errorf("parse %q: group=%q adb=%q err=%v", tc.args, group, args, err)
		}
	}
}

func TestGroupADBCLI(t *testing.T) {
	// A short HOME keeps the daemon's Unix socket path within macOS limits.
	home, err := os.MkdirTemp("/tmp", "adg-adb-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	clientDir := filepath.Join(home, ".config", "adbgroup")
	ca, err := pki.Ensure(filepath.Join(home, "pki"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pki.IssueClient(ca.CA, ca.CAKey, "spiffe://adbgroup/alice", clientDir, 1); err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(ca.Certificate, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(mustRead(t, ca.CA))
	cfg := gateway.Config{
		Groups:     []gateway.Group{{Name: "a"}, {Name: "devices"}, {Name: "restricted"}},
		Identities: map[string][]string{"spiffe://adbgroup/alice": {"a", "devices"}},
	}
	cfg.Defaults()
	engine := gateway.NewEngine(cfg)
	gw := gateway.NewGateway(engine, gateway.Backend{})
	defer gw.Forwards.Close()
	remote := tunnel.NewServer(context.Background(), gw)
	defer remote.Close()
	server := httptest.NewUnstartedServer((gateway.API{Engine: engine, Tunnels: remote.Handler()}).Handler())
	server.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	server.StartTLS()
	defer server.Close()
	configPath := filepath.Join(clientDir, "tunnel.yaml")
	if err := os.WriteFile(configPath, []byte("server: "+server.URL+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	const adbScript = `#!/bin/sh
case "$FAKE_ADB_MODE" in
    args) printf '%s\000' "$@" ;;
    streams) printf 'stderr from adb\n' >&2; /bin/cat; exit 37 ;;
    tty) [ -t 0 ] && [ -t 1 ] && [ -t 2 ] && printf 'adb has tty\n' ;;
    interrupt)
        trap 'exit 42' INT
        printf 'ready\n'
        read -r line
        exit 91
        ;;
esac
`
	if err := os.WriteFile(filepath.Join(home, "adb"), []byte(adbScript), 0700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// This is a valid existing shell selection pointing to a different gateway.
	// Any accidental use of that selection will fail during tunnel setup.
	stale := selection{ID: "old", Config: daemon.Config{
		Server: "https://127.0.0.1:1", Group: "devices", Listen: "127.0.0.1:5037",
		Cert: filepath.Join(clientDir, "client.pem"), Key: filepath.Join(clientDir, "client.key"), CA: ca.CA,
	}}
	selectionJSON, err := json.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "HOME="+home, "PATH="+home, "ADBGROUP_TEST_CLI=1",
		"ADB_SERVER_SOCKET=tcp:"+stale.Config.Listen, "ADBGROUP_CONTEXT="+string(selectionJSON))
	command := func(args ...string) *exec.Cmd {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		t.Cleanup(cancel)
		cmd := exec.CommandContext(ctx, executable, args...)
		cmd.Env = append(env, "FAKE_ADB_MODE=args")
		return cmd
	}
	defer func() {
		if output, err := command("daemon", "stop").CombinedOutput(); err != nil {
			t.Errorf("daemon stop: %s (%v)", output, err)
		}
	}()

	missing := command("a", "devices")
	missing.Env = append(missing.Env, "PATH="+t.TempDir())
	if output, err := missing.CombinedOutput(); err == nil || !strings.Contains(string(output), "adb") {
		t.Fatalf("missing adb: %q (%v)", output, err)
	}
	if _, err := os.Stat(filepath.Join(clientDir, "daemon")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing adb created daemon state: %v", err)
	}

	// Run different selector spellings concurrently. The fake adb records exact
	// argument boundaries, including @ in the device command.
	groups := []string{"a", "a", "devices", "devices"}
	prefixes := [][]string{{"a"}, {"-f", configPath, "@a"}, {"@devices"}, {"-g", "devices"}}
	nativeArgs := []string{"-s", "serial", "shell", "echo", "", "two words", "$(false)", "-P", "@file"}
	var calls []*exec.Cmd
	outputs := make([]bytes.Buffer, len(groups))
	for i, prefix := range prefixes {
		cmd := command(append(prefix, nativeArgs...)...)
		if i == 0 {
			cmd.Env = append(cmd.Env, "ADBGROUP_CONTEXT=", "ADB_SERVER_SOCKET=")
		}
		cmd.Stdout, cmd.Stderr = &outputs[i], &outputs[i]
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, cmd)
	}
	for i, cmd := range calls {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("group %s: %s (%v)", groups[i], outputs[i].String(), err)
		}
	}
	list, err := command("t", "list", "--json").Output()
	if err != nil {
		t.Fatal(err)
	}
	var entries []daemon.Tunnel
	if err := json.Unmarshal(list, &entries); err != nil || len(entries) != 2 {
		t.Fatalf("expected two tunnels: %s (%v)", list, err)
	}
	listeners := make(map[string]string)
	for _, entry := range entries {
		listeners[entry.Group] = entry.Listen
	}
	if listeners["a"] == listeners["devices"] {
		t.Fatalf("groups share a listener: %v", listeners)
	}
	for i, group := range groups {
		host, port, err := net.SplitHostPort(listeners[group])
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{"-H", host, "-P", port}, nativeArgs...)
		got := strings.Split(strings.TrimSuffix(outputs[i].String(), "\x00"), "\x00")
		if !reflect.DeepEqual(got, want) {
			t.Errorf("group %s argv = %q, want %q", group, got, want)
		}
	}

	denied := command("restricted", "devices")
	if stdout, err := denied.Output(); err == nil || len(stdout) != 0 {
		t.Fatalf("unauthorized call ran adb: stdout=%q err=%v", stdout, err)
	}
	streams := command("a", "shell")
	streams.Env = append(streams.Env, "FAKE_ADB_MODE=streams")
	input := "binary\x00input\n"
	streams.Stdin = strings.NewReader(input)
	var stderr bytes.Buffer
	streams.Stderr = &stderr
	stdout, err := streams.Output()
	var status *exec.ExitError
	if !errors.As(err, &status) || status.ExitCode() != 37 || string(stdout) != input || stderr.String() != "stderr from adb\n" {
		t.Fatalf("stdout=%q stderr=%q status=%v", stdout, stderr.String(), err)
	}

	interactive := command("a", "shell")
	interactive.Env = append(interactive.Env, "FAKE_ADB_MODE=interrupt")
	stdin, err := interactive.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	ready, err := interactive.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := interactive.Start(); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(ready).ReadString('\n')
	if err != nil || line != "ready\n" {
		t.Fatalf("waiting for adb: %q (%v)", line, err)
	}
	if err := interactive.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := interactive.Wait(); !errors.As(err, &status) || status.ExitCode() != 42 {
		t.Fatalf("interrupt did not reach adb: %v", err)
	}

	// Run under the same PTY utility used by the shell integration tests.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var pty *exec.Cmd
	if runtime.GOOS == "darwin" {
		pty = exec.CommandContext(ctx, "script", "-q", "/dev/null", executable, "a", "shell")
	} else {
		pty = exec.CommandContext(ctx, "script", "-q", "-e", "-c", `"$ADG_EXECUTABLE" a shell`, "/dev/null")
	}
	pty.Env = append(env, "ADG_EXECUTABLE="+executable, "FAKE_ADB_MODE=tty", "SHELL=/bin/sh")
	if output, err := pty.CombinedOutput(); err != nil || !strings.Contains(string(output), "adb has tty") {
		t.Fatalf("adb under PTY: %q (%v)", output, err)
	}
}
