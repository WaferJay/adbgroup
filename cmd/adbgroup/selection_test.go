package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/daemon"
	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
	"github.com/WaferJay/adbgroup/internal/tunnel"
)

type menuOutput struct {
	buffer bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func (w *menuOutput) Write(p []byte) (int, error) {
	n, err := w.buffer.Write(p)
	if strings.Contains(w.buffer.String(), "Choose group (number, Enter/q to cancel):") {
		w.once.Do(func() { close(w.ready) })
	}
	return n, err
}

func TestSelectionContextAndOverrides(t *testing.T) {
	cfg := daemon.Config{Server: "https://adb.example", Group: "selected", Listen: "127.0.0.1:15038", Cert: "/a ' b/client.pem", Key: "/a ' b/client.key", CA: "/a ' b/ca.pem"}
	data, _ := json.Marshal(selection{Config: cfg, ID: "test"})
	t.Setenv("ADBGROUP_CONTEXT", string(data))
	t.Setenv("ADB_SERVER_SOCKET", "tcp:"+cfg.Listen)
	for _, args := range [][]string{nil, {"-g", "explicit"}, {"-s", "https://other.example"}, {"-f", "other.yaml"}, {"-d", "/other"}, {"-cert", "/other.pem"}} {
		flags := newCommandFlags("groups")
		o := bindClientFlags(flags)
		flags.String("f", "", "config")
		if err := flags.Parse(args); err != nil {
			t.Fatal(err)
		}
		active := applySelection(o, flags)
		if len(args) == 0 || args[0] == "-g" {
			if active == nil || o.server != cfg.Server || o.cert != cfg.Cert {
				t.Fatalf("selection not applied: %+v", o)
			}
			want := cfg.Group
			if len(args) > 0 {
				want = "explicit"
			}
			if o.group != want {
				t.Fatalf("group = %q, want %q", o.group, want)
			}
		} else if active != nil {
			t.Fatalf("explicit connection did not override context: %v", args)
		}
	}
	t.Setenv("ADB_SERVER_SOCKET", "tcp:127.0.0.1:5037")
	if currentSelection() != nil {
		t.Fatal("mismatched socket accepted")
	}
	t.Setenv("ADBGROUP_CONTEXT", "invalid")
	if currentSelection() != nil {
		t.Fatal("invalid context accepted")
	}
}

func TestMenuChoicesAndGroupMarkers(t *testing.T) {
	groups := []gateway.Group{{Name: "a"}, {Name: "b"}}
	if got, err := parseGroupChoice(groups, " 2\n"); err != nil || got != "b" {
		t.Fatalf("choice: %q %v", got, err)
	}
	for _, input := range []string{"", "q", "0", "3", "a", "$(touch bad)"} {
		if _, err := parseGroupChoice(groups, input); err == nil {
			t.Fatalf("invalid choice accepted: %q", input)
		}
	}
	for _, tc := range []struct{ current, rowA, rowB string }{
		{"b", "  a ", "* b "},
		{"a", "* a ", "  b "},
		{"", "  a ", "  b "},
	} {
		var out bytes.Buffer
		if err := runQuery(context.Background(), &inventoryStub{groups: groups}, "groups", "a", tc.current, false, &out); err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
		if len(lines) != 3 || !strings.HasPrefix(lines[0], "  NAME ") ||
			!strings.HasPrefix(lines[1], tc.rowA) || !strings.HasPrefix(lines[2], tc.rowB) {
			t.Errorf("unexpected group markers for current %q:\n%s", tc.current, out.String())
		}
	}
}

func TestShellSwitchWithMTLS(t *testing.T) {
	for _, name := range []string{"zsh", "bash", "script"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("shell integration tests require %s in PATH: %v", name, err)
		}
	}
	home, err := os.MkdirTemp("/tmp", "adbgroup-use-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(home)
	files, err := pki.Ensure(filepath.Join(home, "pki"), []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	creds := filepath.Join(home, "client ' $(false)")
	if _, err := pki.IssueClient(files.CA, files.CAKey, "spiffe://adbgroup/alice", creds, 1); err != nil {
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
	cfg := gateway.Config{
		Groups:     []gateway.Group{{Name: "source", Devices: []string{"DEVICES"}}, {Name: "devices", Devices: []string{"DEVICE_B"}}},
		AutoGroups: gateway.AutoGroupsConfig{Serial: gateway.SerialGroupConfig{Enabled: true, Source: "source"}},
		Identities: map[string][]string{"spiffe://adbgroup/alice": {"DEVICES", "devices"}},
	}
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
	bin := filepath.Join(home, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(bin, "adg")); err != nil {
		t.Fatal(err)
	}
	defaultConfig := filepath.Join(home, tunnelConfigPath)
	if err := os.MkdirAll(filepath.Dir(defaultConfig), 0700); err != nil {
		t.Fatal(err)
	}
	clientConfig := fmt.Sprintf("server: %q\ncredentials: %q\n", s.URL, creds)
	if err := os.WriteFile(defaultConfig, []byte(clientConfig), 0600); err != nil {
		t.Fatal(err)
	}
	testConfig := filepath.Join(home, "client profile.yaml")
	if err := os.WriteFile(testConfig, []byte(clientConfig), 0600); err != nil {
		t.Fatal(err)
	}
	const adb = `#!/bin/sh
set -eu
[ "${ADBGROUP_SHELL_STATE_FILE-}" = "" ]
[ "$#" = 6 ]
[ "$1" = -H ]
[ "$3" = -P ]
[ "tcp:$2:$4" = "$EXPECTED_ADB_SOCKET" ]
[ "$5" = devices ]
[ "$6" = -l ]
exit 37
`
	if err := os.WriteFile(filepath.Join(bin, "adb"), []byte(adb), 0700); err != nil {
		t.Fatal(err)
	}
	// Each shell command runs its own race-instrumented test binary.
	raceOptions := strings.TrimSpace(os.Getenv("GORACE") + " atexit_sleep_ms=0")
	env := append(os.Environ(), "ADBGROUP_TEST_CLI=1", "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "TEST_SERVER="+s.URL, "TEST_CREDS="+creds, "TEST_CONFIG="+testConfig, "GORACE="+raceOptions)
	defer func() {
		cmd := exec.Command(executable, "daemon", "stop")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("daemon cleanup: %v: %s", err, out)
		}
	}()
	t.Run("inventory without shell integration", func(t *testing.T) {
		for _, args := range [][]string{{"devices"}, {"t"}, {"t", "--no-daemon"}, {"t", "-w", "lease"}, {"--shell-client", "d"}, {"--shell-client", "t"}} {
			cmd := exec.Command(executable, args...)
			cmd.Env = append(env, "ADBGROUP_CONTEXT=", shellStateFile+"=")
			if out, err := cmd.CombinedOutput(); err == nil || !strings.Contains(string(out), "group required") {
				t.Fatalf("missing group %v: %v %s", args, err, out)
			}
		}
		cmd := exec.Command(executable, "devices", "DEVICES", "--json")
		cmd.Env = append(env, "ADBGROUP_CONTEXT=", shellStateFile+"=")
		out, err := cmd.Output()
		var devices []gateway.Device
		if err != nil || json.Unmarshal(out, &devices) != nil || len(devices) != 1 || devices[0].Serial != "DEVICES" {
			t.Fatalf("explicit group inventory: %v %s", err, out)
		}
		cmd = exec.Command(executable, "daemon", "status")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil || strings.TrimSpace(string(out)) != "daemon stopped" {
			t.Fatalf("inventory started a daemon: %v %s", err, out)
		}
	})
	for _, shell := range []string{"zsh", "bash"} {
		for _, original := range []string{"unset", "empty", "custom"} {
			t.Run(shell+"/"+original, func(t *testing.T) {
				path, err := exec.LookPath(shell)
				if err != nil {
					t.Fatal(err)
				}
				tmp := t.TempDir()
				script := `set -eu
case "$2" in
  unset) unset ADB_SERVER_SOCKET ;;
  empty) export ADB_SERVER_SOCKET='' ;;
  custom) export ADB_SERVER_SOCKET='tcp:127.0.0.1:9999' ;;
esac
unset ADBGROUP_CONTEXT ADBGROUP_GROUP ADBGROUP_SHELL_STATE_FILE
original_set="${ADB_SERVER_SOCKET+x}"
original_value="${ADB_SERVER_SOCKET-}"
eval "$(command adg shell-init "$1")"
eval "$(command adg shell-init "$1")"
adg g >/dev/null
adg devices --help >/dev/null 2>&1
adg t list --json >/dev/null
adg t stop --help >/dev/null 2>&1
if adg devices shell extra >/dev/null 2>&1; then exit 17; fi
adg devices @devices --json > "$TEST_OUTPUT"
grep -q DEVICE_B "$TEST_OUTPUT"
if adg devices -f "$TEST_CONFIG.missing" >/dev/null 2>&1; then exit 16; fi
[ "${ADBGROUP_CONTEXT+x}" != x ]
[ "${ADBGROUP_GROUP+x}" != x ]
[ "${ADB_SERVER_SOCKET+x}" = "$original_set" ]
[ "${ADB_SERVER_SOCKET-}" = "$original_value" ]
if adg d -f "$TEST_CONFIG" --json > "$TEST_OUTPUT" 2>&1; then exit 21; fi
grep -q "group required" "$TEST_OUTPUT"
adg d @devices -f "$TEST_CONFIG" --json > "$TEST_OUTPUT"
grep -q DEVICE_B "$TEST_OUTPUT"
adg use @devices -f "$TEST_CONFIG"
[ "$ADBGROUP_GROUP" = devices ]
inventory_socket=$ADB_SERVER_SOCKET
adg unuse
adb_status=0
EXPECTED_ADB_SOCKET="$inventory_socket" adg @devices devices -l || adb_status=$?
[ "$adb_status" = 37 ]
[ "${ADBGROUP_CONTEXT+x}" != x ]
[ "${ADBGROUP_GROUP+x}" != x ]
if adg d --json > "$TEST_OUTPUT" 2>&1; then exit 22; fi
grep -q "adg devices GROUP" "$TEST_OUTPUT"
adg use DEVICES
command adg devices --json > "$TEST_OUTPUT.expected"
adg devices --json > "$TEST_OUTPUT"
cmp "$TEST_OUTPUT.expected" "$TEST_OUTPUT"
[ "$ADBGROUP_GROUP" = DEVICES ]
first_socket=$ADB_SERVER_SOCKET
first_context=$ADBGROUP_CONTEXT
if adg d -f "$TEST_CONFIG" --json > "$TEST_OUTPUT" 2>&1; then exit 21; fi
grep -q "group required" "$TEST_OUTPUT"
adg d @devices -f "$TEST_CONFIG" --json > "$TEST_OUTPUT"
grep -q DEVICE_B "$TEST_OUTPUT"
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
adg -f "$TEST_CONFIG" DEVICES
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
[ "$(sh -c 'printf %s "$ADB_SERVER_SOCKET"')" = "$first_socket" ]
[ "$(sh -c 'printf %s "$ADBGROUP_GROUP"')" = DEVICES ]
case "$(adg d --json)" in *DEVICES*) ;; *) exit 10 ;; esac
adg @devices
[ "$ADBGROUP_GROUP" = devices ]
[ "$ADB_SERVER_SOCKET" != "$first_socket" ]
second_socket=$ADB_SERVER_SOCKET
second_context=$ADBGROUP_CONTEXT
case "$(adg devices)" in *DEVICE_B*) ;; *) exit 11 ;; esac
case "$(adg d --json DEVICES)" in *DEVICES*) ;; *) exit 12 ;; esac
adb_status=0
EXPECTED_ADB_SOCKET="$first_socket" adg -f "$TEST_CONFIG" @DEVICES devices -l || adb_status=$?
[ "$adb_status" = 37 ]
[ "$ADBGROUP_GROUP" = devices ]
[ "$ADB_SERVER_SOCKET" = "$second_socket" ]
[ "$ADBGROUP_CONTEXT" = "$second_context" ]
adg g | grep -q '^[*] devices ' || exit 13
case "$1" in
  bash) shopt -s nocasematch ;;
  zsh) setopt nocasematch ;;
esac
adg DEVICES
[ "$ADBGROUP_GROUP" = DEVICES ]
[ "$ADB_SERVER_SOCKET" = "$first_socket" ]
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
if adg forbidden; then exit 15; fi
[ "$ADBGROUP_GROUP" = DEVICES ]
[ "$ADB_SERVER_SOCKET" = "$first_socket" ]
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
if adg use DEVICES -s http://invalid.example; then exit 14; fi
[ "$ADBGROUP_GROUP" = DEVICES ]
[ "$ADB_SERVER_SOCKET" = "$first_socket" ]
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
adg -f="$TEST_CONFIG" --group=devices
[ "$ADBGROUP_CONTEXT" = "$second_context" ]
adg use @DEVICES -s "$TEST_SERVER" -d "$TEST_CREDS"
if adg t -l "${first_socket#tcp:}" > "$TEST_OUTPUT" 2>&1; then exit 18; fi
grep -q "adg tunnel GROUP" "$TEST_OUTPUT"
if adg t @devices -l "${first_socket#tcp:}" > "$TEST_OUTPUT" 2>&1; then exit 23; fi
grep -q "different configuration" "$TEST_OUTPUT"
adg t -s "$TEST_SERVER" -d "$TEST_CREDS" -g devices -l "${second_socket#tcp:}"
[ "$ADB_SERVER_SOCKET" = "$first_socket" ]
[ "$ADBGROUP_GROUP" = DEVICES ]
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
adg unuse
[ "${ADB_SERVER_SOCKET+x}" = "$original_set" ]
[ "${ADB_SERVER_SOCKET-}" = "$original_value" ]
[ "${ADBGROUP_CONTEXT+x}" != x ]
[ "${ADBGROUP_GROUP+x}" != x ]
if adg t DEVICES -l "${second_socket#tcp:}" > "$TEST_OUTPUT" 2>&1; then exit 24; fi
grep -q "different configuration" "$TEST_OUTPUT"
adg t DEVICES -l "${first_socket#tcp:}" > "$TEST_OUTPUT"
grep -Fq "group=DEVICES listen=${first_socket#tcp:}" "$TEST_OUTPUT"
[ "${ADBGROUP_CONTEXT+x}" != x ]
[ "${ADB_SERVER_SOCKET+x}" = "$original_set" ]
[ "${ADB_SERVER_SOCKET-}" = "$original_value" ]
adg unuse
[ "${ADBGROUP_GROUP+x}" != x ]
printf '\nSHELL_SWITCH_OK\n'
`
				commandCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				args := []string{"--noprofile", "--norc", "-c", script, "test", shell, original}
				if shell == "zsh" {
					args = []string{"-f", "-c", script, "test", shell, original}
				}
				cmd := exec.CommandContext(commandCtx, path, args...)
				cmd.Env = append(env, "TMPDIR="+tmp, "TEST_OUTPUT="+filepath.Join(t.TempDir(), "output"))
				out, err := cmd.CombinedOutput()
				if err != nil || !strings.Contains(string(out), "SHELL_SWITCH_OK") {
					t.Fatalf("shell: %v\n%s", err, out)
				}
				entries, err := os.ReadDir(tmp)
				if err != nil || len(entries) != 0 {
					t.Fatalf("temporary files leaked: %v %v", entries, err)
				}
			})
		}
	}
	// A PTY exercises /dev/tty selection, including cancellation, in both shells.
	for _, shell := range []string{"zsh", "bash"} {
		for _, tc := range []struct{ command, mode string }{
			{"adg", "select"}, {"adg", "cancel"}, {"adg", "interrupt"},
			{"adg use", "select"}, {"adg use", "cancel"}, {"adg use", "interrupt"},
		} {
			t.Run(fmt.Sprintf("menu/%s/%s/%s", shell, tc.command, tc.mode), func(t *testing.T) {
				cancelMenu := tc.mode != "select"
				tmp := t.TempDir()
				pidFile := filepath.Join(t.TempDir(), "use.pid")
				scriptPath := filepath.Join(t.TempDir(), "menu.sh")
				script := `set -eu
export ADB_SERVER_SOCKET=original
unset ADBGROUP_CONTEXT
export ADBGROUP_GROUP=original
eval "$(command adg shell-init "$TEST_SHELL")"
if ` + tc.command + `; then
    [ "$TEST_CANCEL" = false ]
    [ "$ADBGROUP_GROUP" = devices ]
    case "$(adg d --json)" in *DEVICE_B*) ;; *) exit 20 ;; esac
else
    [ "$TEST_CANCEL" = true ]
    [ "$ADB_SERVER_SOCKET" = original ]
    [ "$ADBGROUP_GROUP" = original ]
    [ "${ADBGROUP_CONTEXT+x}" != x ]
fi
printf '\nMENU_OK\n'
`
				if err := os.WriteFile(scriptPath, []byte(script), 0600); err != nil {
					t.Fatal(err)
				}
				ctx, done := context.WithTimeout(context.Background(), 15*time.Second)
				defer done()
				var cmd *exec.Cmd
				if runtime.GOOS == "darwin" {
					cmd = exec.CommandContext(ctx, "script", "-q", "/dev/null", shell, scriptPath)
				} else {
					quoted := "'" + strings.ReplaceAll(scriptPath, "'", "'\\''") + "'"
					cmd = exec.CommandContext(ctx, "script", "-q", "-e", "-c", shell+" "+quoted, "/dev/null")
				}
				cmd.Env = append(env, "TEST_SHELL="+shell, fmt.Sprintf("TEST_CANCEL=%v", cancelMenu), "TMPDIR="+tmp, "ADBGROUP_TEST_PID_FILE="+pidFile)
				input := "2\n"
				if cancelMenu {
					input = "q\n"
				}
				output := &menuOutput{ready: make(chan struct{})}
				cmd.Stdout, cmd.Stderr = output, output
				stdin, err := cmd.StdinPipe()
				if err != nil {
					t.Fatal(err)
				}
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				select {
				case <-output.ready:
					if tc.mode == "interrupt" {
						data, readErr := os.ReadFile(pidFile)
						if readErr != nil {
							t.Fatal(readErr)
						}
						pid, parseErr := strconv.Atoi(string(data))
						if parseErr != nil {
							t.Fatal(parseErr)
						}
						process, findErr := os.FindProcess(pid)
						if findErr != nil {
							t.Fatal(findErr)
						}
						err = process.Signal(os.Interrupt)
					} else {
						_, err = io.WriteString(stdin, input)
					}
				case <-ctx.Done():
					err = ctx.Err()
				}
				waitErr := cmd.Wait()
				stdin.Close()
				out := output.buffer.String()
				if err != nil || waitErr != nil || !strings.Contains(out, "MENU_OK") || tc.mode == "cancel" && !strings.Contains(out, "group selection cancelled") {
					t.Fatalf("menu: %v %v\n%s", err, waitErr, out)
				}
				entries, err := os.ReadDir(tmp)
				if err != nil || len(entries) != 0 {
					t.Fatalf("menu temporary files leaked: %v %v", entries, err)
				}
			})
		}
	}
	t.Run("no tty", func(t *testing.T) {
		state, err := os.CreateTemp(t.TempDir(), "state")
		if err != nil {
			t.Fatal(err)
		}
		state.Close()
		for _, args := range [][]string{nil, {"use"}} {
			cmd := exec.Command(executable, args...)
			cmd.Env = append(env, "ADBGROUP_CONTEXT=", shellStateFile+"="+state.Name())
			cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
			out, err := cmd.CombinedOutput()
			if err == nil || !strings.Contains(string(out), "specify a group") {
				t.Fatalf("no tty for adg %v: %v %s", args, err, out)
			}
		}
	})
	cmd := exec.Command(executable, "t", "list", "--json")
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var entries []daemon.Tunnel
	if err := json.Unmarshal(out, &entries); err != nil || len(entries) != 3 {
		t.Fatalf("tunnels: %s (%v)", out, err)
	}
	ports := map[string]bool{}
	for _, entry := range entries {
		if entry.State == "active" {
			if ports[entry.Listen] {
				t.Fatal("different groups share a listener")
			}
			ports[entry.Listen] = true
		} else if entry.Group != "forbidden" || entry.State != "failed" {
			t.Fatalf("unexpected entry: %+v", entry)
		}
	}
	if len(ports) != 2 {
		t.Fatalf("active listeners: %v", ports)
	}
}
