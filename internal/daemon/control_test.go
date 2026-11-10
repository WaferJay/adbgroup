package daemon

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The test executable also acts as a real detached daemon for launch tests.
func TestMain(m *testing.M) {
	if os.Getenv("ADBGROUP_TEST_DAEMON_DIR") != "" && len(os.Args) == 3 && os.Args[1] == "daemon" && os.Args[2] == "run" {
		ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
		defer cancel()
		if err := Run(ctx, os.Getenv("ADBGROUP_TEST_DAEMON_DIR")); err != nil {
			os.Stderr.WriteString(err.Error())
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func shortDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "adbgroup-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

func TestControlProtocolAndShutdown(t *testing.T) {
	dir := shortDir(t)
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	m := newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		ready(cfg.Group, cfg.Listen)
		<-ctx.Done()
		return nil
	})
	ctx, cancel := context.WithCancel(testContext(t))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, m) }()
	defer func() { cancel(); <-done }()
	c, err := net.Dial("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	json.NewEncoder(c).Encode(Request{Version: ProtocolVersion + 1, Operation: "create"})
	var rejected Response
	if err := json.NewDecoder(c).Decode(&rejected); err != nil {
		t.Fatal(err)
	}
	c.Close()
	if rejected.Error == "" || len(m.List()) != 0 {
		t.Fatal("version mismatch accepted")
	}
	cfg := testConfig("15037")
	created, err := Call(ctx, dir, Request{Operation: "create", Config: &cfg})
	if err != nil || created.Tunnel == nil || created.Tunnel.State != "active" {
		t.Fatalf("create: %+v, %v", created, err)
	}
	listed, err := Call(ctx, dir, Request{Operation: "list"})
	if err != nil || len(listed.Tunnels) != 1 {
		t.Fatalf("list: %+v, %v", listed, err)
	}
	if _, err := Call(ctx, dir, Request{Operation: "shutdown"}); err != nil {
		t.Fatal(err)
	}
	if len(m.List()) != 0 {
		t.Fatal("shutdown replied before cleanup")
	}
}

func TestConcurrentEnsureDetachedProcessAndStaleSocket(t *testing.T) {
	dir := shortDir(t)
	t.Setenv("ADBGROUP_TEST_DAEMON_DIR", dir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a socket left behind by an abruptly terminated process.
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: SocketPath(dir), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	l.SetUnlinkOnClose(false)
	l.Close()
	ctx := testContext(t)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		Call(cleanup, dir, Request{Operation: "shutdown"})
		WaitStopped(cleanup, dir)
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := Ensure(ctx, dir, executable); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	status, err := Call(ctx, dir, Request{Operation: "status"})
	if err != nil {
		t.Fatal(err)
	}
	if status.PID == os.Getpid() || status.PID == 0 {
		t.Fatal("daemon was not a separate process")
	}
	sid, err := syscall.Getsid(status.PID)
	if err != nil || sid != status.PID {
		t.Fatalf("daemon is not detached: sid=%d pid=%d err=%v", sid, status.PID, err)
	}
	st, err := os.Stat(SocketPath(dir))
	if err != nil || st.Mode().Perm() != 0600 {
		t.Fatalf("socket permissions: %v, %v", st, err)
	}
	if _, err := Call(ctx, dir, Request{Operation: "shutdown"}); err != nil {
		t.Fatal(err)
	}
	if err := WaitStopped(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(SocketPath(dir)); !os.IsNotExist(err) {
		t.Fatalf("socket survived shutdown: %v", err)
	}
	if err := Ensure(ctx, dir, executable); err != nil {
		t.Fatal(err)
	}
	listed, err := Call(ctx, dir, Request{Operation: "list"})
	if err != nil || len(listed.Tunnels) != 0 {
		t.Fatal("restarted daemon was not empty")
	}
}

func TestPrivateDirectoryRequired(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := prepareDir(dir); err == nil {
		t.Fatal("public directory accepted")
	}
}

func TestDisconnectedClientCancelsPendingSetup(t *testing.T) {
	dir := shortDir(t)
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cleaned := make(chan struct{})
	m := newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		<-ctx.Done()
		close(cleaned)
		return ctx.Err()
	})
	ctx, cancel := context.WithCancel(testContext(t))
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, m) }()
	defer func() { cancel(); <-done }()
	c, err := net.Dial("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	cfg := testConfig("15037")
	if err := json.NewEncoder(c).Encode(Request{Version: ProtocolVersion, Operation: "create", Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return len(m.List()) == 1 })
	c.Close()
	select {
	case <-cleaned:
	case <-ctx.Done():
		t.Fatal("disconnected client left pending setup running")
	}
}

func TestClientRejectsIncompatibleDaemon(t *testing.T) {
	dir := shortDir(t)
	l, err := net.Listen("unix", SocketPath(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		var req Request
		json.NewDecoder(c).Decode(&req)
		json.NewEncoder(c).Encode(Response{Version: ProtocolVersion + 1})
	}()
	if _, err := Call(testContext(t), dir, Request{Operation: "status"}); err == nil {
		t.Fatal("incompatible daemon accepted")
	}
	<-done
}
