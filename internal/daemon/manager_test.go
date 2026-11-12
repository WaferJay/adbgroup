package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig(port string) Config {
	return Config{Server: "https://adb.example/", Group: "a", Listen: port, Cert: "/credentials/client.pem", Key: "/credentials/client.key", CA: "/credentials/ca.pem"}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func eventually(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("condition timed out")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestManagerConcurrentReuseConflictAndIndependentStop(t *testing.T) {
	var runs atomic.Int32
	m := newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		runs.Add(1)
		ready(cfg.Group, cfg.Listen)
		<-ctx.Done()
		return nil
	})
	defer m.Close()
	ctx := testContext(t)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := m.Start(ctx, testConfig("15037"))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- info.ID
		}()
	}
	wg.Wait()
	close(ids)
	var id string
	for next := range ids {
		if id != "" && next != id {
			t.Fatal("duplicate tunnel created")
		}
		id = next
	}
	if runs.Load() != 1 {
		t.Fatalf("runs = %d", runs.Load())
	}
	conflict := testConfig("127.0.0.1:15037")
	conflict.Group = "b"
	if _, err := m.Start(ctx, conflict); err == nil {
		t.Fatal("conflicting config accepted")
	}
	second, err := m.Start(ctx, testConfig("15038"))
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Stop(ctx, id); err != nil {
		t.Fatal(err)
	}
	list := m.List()
	if len(list) != 1 || list[0].ID != second.ID || list[0].State != "active" {
		t.Fatalf("stop affected second tunnel: %+v", list)
	}
	if err := m.Stop(ctx, id); err == nil {
		t.Fatal("missing ID accepted")
	}
	m.Close()
	if _, err := m.Start(ctx, testConfig("15039")); err == nil {
		t.Fatal("closed manager accepted tunnel")
	}
}

func TestManagerFailureIsolationAndExplicitRetry(t *testing.T) {
	fail := make(chan struct{})
	var attempts atomic.Int32
	m := newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		if cfg.Listen == "127.0.0.1:15037" && attempts.Add(1) == 1 {
			ready("a", cfg.Listen)
			select {
			case <-fail:
				return errors.New("control disconnected")
			case <-ctx.Done():
				return nil
			}
		}
		ready("a", cfg.Listen)
		<-ctx.Done()
		return nil
	})
	defer m.Close()
	ctx := testContext(t)
	first, err := m.Start(ctx, testConfig("15037"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Start(ctx, testConfig("15038"))
	if err != nil {
		t.Fatal(err)
	}
	close(fail)
	eventually(t, func() bool {
		for _, info := range m.List() {
			if info.ID == first.ID {
				return info.State == "failed" && info.Error == "control disconnected"
			}
		}
		return false
	})
	if attempts.Load() != 1 {
		t.Fatal("automatically reconnected")
	}
	for _, info := range m.List() {
		if info.ID == second.ID && info.State != "active" {
			t.Fatal("failure affected another tunnel")
		}
	}
	retried, err := m.Start(ctx, testConfig("15037"))
	if err != nil || retried.ID != first.ID || attempts.Load() != 2 {
		t.Fatalf("manual retry: %+v, %v", retried, err)
	}
}

func TestManagerTimeoutCancellationAndCleanup(t *testing.T) {
	var cleaned atomic.Int32
	m := newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		<-ctx.Done()
		cleaned.Add(1)
		return ctx.Err()
	})
	m.setupTimeout = 20 * time.Millisecond
	defer m.Close()
	info, err := m.Start(testContext(t), testConfig("15037"))
	if err == nil || !strings.Contains(err.Error(), "timed out") || info.State != "failed" || cleaned.Load() != 1 {
		t.Fatalf("timeout: %+v, %v, cleaned=%d", info, err, cleaned.Load())
	}
	ctx, cancel := context.WithCancel(testContext(t))
	started := make(chan error, 1)
	go func() { _, err := m.Start(ctx, testConfig("15038")); started <- err }()
	eventually(t, func() bool { return len(m.List()) == 2 })
	cancel()
	if err := <-started; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	eventually(t, func() bool { return cleaned.Load() == 2 })
	if err := m.Stop(testContext(t), info.ID); err != nil {
		t.Fatal(err)
	}
}

func TestManagerRejectsInvalidConfiguration(t *testing.T) {
	m := newManager(func(context.Context, Config, func(string, string)) error {
		t.Error("invalid config reached runner")
		return nil
	})
	defer m.Close()
	for _, update := range []func(*Config){
		func(c *Config) { c.Listen = "0.0.0.0:15037" },
		func(c *Config) { c.Server = "http://adb.example" },
		func(c *Config) { c.Cert = "relative.pem" },
	} {
		cfg := testConfig("15037")
		update(&cfg)
		if _, err := m.Start(testContext(t), cfg); err == nil {
			t.Fatal("invalid config accepted")
		}
	}
}

func TestEnsureConcurrentReuseAndIsolation(t *testing.T) {
	var runs atomic.Int32
	m := newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		n := runs.Add(1)
		address := cfg.Listen
		if address == "" {
			address = fmt.Sprintf("127.0.0.1:%d", 16000+n)
		}
		ready(cfg.Group, address)
		<-ctx.Done()
		return nil
	})
	defer m.Close()
	ctx := testContext(t)
	var wg sync.WaitGroup
	ids := make(chan string, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			info, err := m.Ensure(ctx, testConfig("ignored"))
			if err != nil {
				t.Error(err)
				return
			}
			ids <- info.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := m.List()[0]
	for id := range ids {
		if id != first.ID {
			t.Fatal("concurrent ensure created duplicate tunnels")
		}
	}
	if runs.Load() != 1 || first.Listen != "127.0.0.1:16001" {
		t.Fatalf("unexpected automatic listener: %+v (%d runs)", first, runs.Load())
	}
	// An explicit start sees the actual port, and can reuse or reject it.
	if info, err := m.Start(ctx, testConfig(first.Listen)); err != nil || info.ID != first.ID {
		t.Fatalf("explicit reuse: %+v %v", info, err)
	}
	conflict := testConfig(first.Listen)
	conflict.Group = "b"
	if _, err := m.Start(ctx, conflict); err == nil {
		t.Fatal("explicit conflict was not rejected")
	}
	for _, update := range []func(*Config){
		func(c *Config) { c.Group = "b" },
		func(c *Config) { c.Server = "https://other.example" },
		func(c *Config) { c.Cert = "/other/client.pem" },
		func(c *Config) { c.Key = "/other/client.key" },
		func(c *Config) { c.CA = "/other/ca.pem" },
	} {
		cfg := testConfig("")
		update(&cfg)
		info, err := m.Ensure(ctx, cfg)
		if err != nil || info.ID == first.ID || info.Listen == first.Listen {
			t.Fatalf("connection isolation: %+v %v", info, err)
		}
	}
	lease := testConfig("15050")
	lease.Lease = "write-lease"
	if _, err := m.Start(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Ensure(ctx, lease); err == nil {
		t.Fatal("ensure accepted a write lease")
	}
	m.Close()
	if _, err := m.Ensure(ctx, testConfig("")); err == nil {
		t.Fatal("closed manager accepted ensure")
	}
}

func TestEnsureRetryAndExistingTunnelPreference(t *testing.T) {
	var attempts atomic.Int32
	m := newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		if attempts.Add(1) == 1 {
			return errors.New("setup failed")
		}
		address := cfg.Listen
		if address == "" {
			address = "127.0.0.1:16001"
		}
		ready(cfg.Group, address)
		<-ctx.Done()
		return nil
	})
	defer m.Close()
	ctx := testContext(t)
	failed, err := m.Ensure(ctx, testConfig(""))
	if err == nil {
		t.Fatal("setup failure was lost")
	}
	retried, err := m.Ensure(ctx, testConfig(""))
	if err != nil || failed.ID != retried.ID || retried.Listen == "" {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	other, err := m.Start(ctx, testConfig("15039"))
	if err != nil {
		t.Fatal(err)
	}
	chosen, err := m.Ensure(ctx, testConfig(""))
	want := min(other.ID, retried.ID)
	if err != nil || chosen.ID != want {
		t.Fatalf("deterministic reuse: %+v want %s: %v", chosen, want, err)
	}
}
