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
	var out bytes.Buffer
	if err := runQuery(context.Background(), &inventoryStub{groups: groups}, "groups", "b", false, &out, groupMarkers{Default: "a", Current: "b"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if !strings.Contains(lines[0], "CURRENT") || !strings.Contains(lines[1], "*") || !strings.Contains(lines[2], "*") {
		t.Fatal(out.String())
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
	cfg := gateway.Config{Groups: []gateway.Group{{Name: "a", Devices: []string{"DEVICE_A"}}, {Name: "b", Devices: []string{"DEVICE_B"}}}, Identities: map[string][]string{"spiffe://adbgroup/alice": {"a", "b"}}}
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
	env := append(os.Environ(), "ADBGROUP_TEST_CLI=1", "HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"), "TEST_SERVER="+s.URL, "TEST_CREDS="+creds)
	defer func() {
		cmd := exec.Command(executable, "daemon", "stop")
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("daemon cleanup: %v: %s", err, out)
		}
	}()
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
unset ADBGROUP_CONTEXT
original_set="${ADB_SERVER_SOCKET+x}"
original_value="${ADB_SERVER_SOCKET-}"
eval "$(command adg shell-init "$1")"
eval "$(command adg shell-init "$1")"
adg use a -s "$TEST_SERVER" -d "$TEST_CREDS"
first_socket=$ADB_SERVER_SOCKET
first_context=$ADBGROUP_CONTEXT
[ "$(sh -c 'printf %s "$ADB_SERVER_SOCKET"')" = "$first_socket" ]
case "$(adg d --json)" in *DEVICE_A*) ;; *) exit 10 ;; esac
adg use b
[ "$ADB_SERVER_SOCKET" != "$first_socket" ]
case "$(adg d --json)" in *DEVICE_B*) ;; *) exit 11 ;; esac
case "$(adg d -g a --json)" in *DEVICE_A*) ;; *) exit 12 ;; esac
case "$(adg g)" in *CURRENT*) ;; *) exit 13 ;; esac
adg use a
[ "$ADB_SERVER_SOCKET" = "$first_socket" ]
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
if adg use forbidden; then exit 15; fi
[ "$ADB_SERVER_SOCKET" = "$first_socket" ]
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
if adg use a -s http://invalid.example; then exit 14; fi
[ "$ADB_SERVER_SOCKET" = "$first_socket" ]
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
command adg t -s "$TEST_SERVER" -d "$TEST_CREDS" -g a -l "${first_socket#tcp:}"
[ "$ADBGROUP_CONTEXT" = "$first_context" ]
adg unuse
[ "${ADB_SERVER_SOCKET+x}" = "$original_set" ]
[ "${ADB_SERVER_SOCKET-}" = "$original_value" ]
[ "${ADBGROUP_CONTEXT+x}" != x ]
adg unuse
printf '\nSHELL_SWITCH_OK\n'
`
				commandCtx, done := context.WithTimeout(context.Background(), 30*time.Second)
				defer done()
				args := []string{"--noprofile", "--norc", "-c", script, "test", shell, original}
				if shell == "zsh" {
					args = []string{"-f", "-c", script, "test", shell, original}
				}
				cmd := exec.CommandContext(commandCtx, path, args...)
				cmd.Env = append(env, "TMPDIR="+tmp)
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
		for _, mode := range []string{"select", "cancel", "interrupt"} {
			t.Run(fmt.Sprintf("menu/%s/%s", shell, mode), func(t *testing.T) {
				cancelMenu := mode != "select"
				tmp := t.TempDir()
				pidFile := filepath.Join(t.TempDir(), "use.pid")
				scriptPath := filepath.Join(t.TempDir(), "menu.sh")
				script := `set -eu
export ADB_SERVER_SOCKET=original
unset ADBGROUP_CONTEXT
eval "$(command adg shell-init "$TEST_SHELL")"
if adg use -s "$TEST_SERVER" -d "$TEST_CREDS"; then
    [ "$TEST_CANCEL" = false ]
    case "$(adg d --json)" in *DEVICE_B*) ;; *) exit 20 ;; esac
else
    [ "$TEST_CANCEL" = true ]
    [ "$ADB_SERVER_SOCKET" = original ]
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
					if mode == "interrupt" {
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
				if err != nil || waitErr != nil || !strings.Contains(out, "MENU_OK") || mode == "cancel" && !strings.Contains(out, "group selection cancelled") {
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
		cmd := exec.Command(executable, "use", "-s", s.URL, "-d", creds)
		cmd.Env = append(env, shellStateFile+"="+state.Name())
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "specify a group") {
			t.Fatalf("no tty: %v %s", err, out)
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
