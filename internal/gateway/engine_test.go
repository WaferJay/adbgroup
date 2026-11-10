package gateway

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

func testConfig() Config {
	c := Config{Backend: BackendConfig{Address: "127.0.0.1:5037"}, API: APIConfig{Listen: "127.0.0.1:8443", Certificate: "server.pem", Key: "server.key", ClientCA: "ca.pem"}, Groups: []Group{{Name: "a", Devices: []string{"A", "B"}}, {Name: "b", Devices: []string{"A", "C"}}}, Identities: map[string][]string{"spiffe://test/alice": {"a"}, "spiffe://test/bob": {"b"}}}
	c.Defaults()
	return c
}
func testDevices() []Device {
	return ParseDevices("A\tdevice usb:1 transport_id:1\nB\tdevice usb:2 transport_id:2\nC\tdevice usb:3 transport_id:3\n")
}

type testCloser struct{ closed atomic.Bool }

func (c *testCloser) Close() error { c.closed.Store(true); return nil }
func engineFixture(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(testConfig())
	e.Update(testDevices())
	return e
}
func selectDevice(t *testing.T, e *Engine, s Scope, serial string) Device {
	t.Helper()
	d, err := e.Select(s, serial, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return d
}
func getRead(t *testing.T, e *Engine, s Scope, serial string, c io.Closer) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := e.Acquire(ctx, s, selectDevice(t, e, s, serial), c)
	if err != nil {
		t.Fatal(err)
	}
	return release
}
func TestConfigValidation(t *testing.T) {
	c := testConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Backend.Address = "5037"
	if err := c.Validate(); err != nil {
		t.Fatalf("bare backend port rejected: %v", err)
	}
	c.Groups[1].Name = c.Groups[0].Name
	if c.Validate() == nil {
		t.Fatal("duplicate groups accepted")
	}
	c = testConfig()
	c.Backend.Address = "0.0.0.0:5037"
	if c.Validate() == nil {
		t.Fatal("public listener accepted")
	}
}

func TestParseBackendAddress(t *testing.T) {
	for _, tc := range []struct {
		input, dial, port string
		all               bool
	}{
		{"5038", "127.0.0.1:5038", "5038", true},
		{"127.0.0.1:5038", "127.0.0.1:5038", "5038", false},
		{"[::1]:5038", "[::1]:5038", "5038", false},
	} {
		got, err := ParseBackendAddress(tc.input)
		if err != nil {
			t.Fatalf("ParseBackendAddress(%q): %v", tc.input, err)
		}
		if got.DialAddress != tc.dial || got.Port != tc.port || got.ListenAll != tc.all {
			t.Fatalf("ParseBackendAddress(%q) = %+v", tc.input, got)
		}
	}
	for _, input := range []string{"0", "65536", "localhost:5038", "0.0.0.0:5038", ":5038", "invalid"} {
		if _, err := ParseBackendAddress(input); err == nil {
			t.Fatalf("invalid backend address %q accepted", input)
		}
	}
}

func TestConfigCertificateDefaults(t *testing.T) {
	c := testConfig()
	c.API.Certificate, c.API.Key, c.API.ClientCA = "", "", ""
	c.PKI.Directory = "/var/lib/adbgroup/pki"
	c.Defaults()
	if !c.PKI.AutoTLS || c.API.Certificate != "/var/lib/adbgroup/pki/server.pem" || c.API.ClientCA != "/var/lib/adbgroup/pki/ca.pem" {
		t.Fatalf("incorrect PKI defaults: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, which := range []string{"certificate", "key", "client_ca"} {
		partial := testConfig()
		partial.API.Certificate, partial.API.Key, partial.API.ClientCA = "", "", ""
		switch which {
		case "certificate":
			partial.API.Certificate = "server.pem"
		case "key":
			partial.API.Key = "server.key"
		case "client_ca":
			partial.API.ClientCA = "ca.pem"
		}
		partial.Defaults()
		if partial.Validate() == nil {
			t.Fatalf("partial TLS config accepted: %s", which)
		}
	}
	external := testConfig()
	if external.PKI.AutoTLS || external.API.Certificate != "server.pem" {
		t.Fatal("explicit credentials replaced")
	}
}
func TestSharedReaderWriterFIFO(t *testing.T) {
	e := engineFixture(t)
	a := Scope{Group: "a", Identity: "spiffe://test/alice"}
	b := Scope{Group: "b", Identity: "spiffe://test/bob"}
	r1 := getRead(t, e, a, "A", &testCloser{})
	r2 := getRead(t, e, b, "A", &testCloser{})
	l1, err := e.CreateLease("spiffe://test/alice", "a", "A", time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := e.CreateLease("spiffe://test/bob", "b", "A", time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if l1.State != "queued" || l2.State != "queued" {
		t.Fatal("writer bypassed readers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err = e.Acquire(ctx, a, selectDevice(t, e, a, "A"), &testCloser{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("new reader bypassed writer: %v", err)
	}
	r1()
	if l, _ := e.Lease(l1.ID); l.State != "queued" {
		t.Fatal("writer ignored other group's reader")
	}
	r2()
	if l, _ := e.Lease(l1.ID); l.State != "active" {
		t.Fatal("first writer not granted")
	}
	s := Scope{Group: "a", Lease: l1.ID, Identity: "spiffe://test/alice"}
	ok := e.AuthorizedScope(s)
	if !ok {
		t.Fatal("lease authentication failed")
	}
	closer := &testCloser{}
	wr := getRead(t, e, s, "A", closer)
	if e.AuthorizedScope(Scope{Group: "a", Lease: l1.ID, Identity: "spiffe://test/bob"}) {
		t.Fatal("bad secret accepted")
	}
	if err = e.ReleaseLease(l1.ID, "spiffe://test/bob"); err == nil {
		t.Fatal("another owner released lease")
	}
	e.ReleaseLease(l1.ID, "spiffe://test/alice")
	waitFor(t, closer.closed.Load)
	if !closer.closed.Load() {
		t.Fatal("writer stream not closed")
	}
	wr()
	waitFor(t, func() bool { l, _ := e.Lease(l2.ID); return l.State == "active" })
	if l, _ := e.Lease(l2.ID); l.State != "active" {
		t.Fatal("FIFO successor not granted")
	}
	e.ReleaseLease(l2.ID, "spiffe://test/bob")
	r := getRead(t, e, b, "A", &testCloser{})
	r()
}
func TestWaitingReaderContinuesAndCancels(t *testing.T) {
	e := engineFixture(t)
	s := Scope{Group: "a", Identity: "spiffe://test/alice"}
	l, _ := e.CreateLease("spiffe://test/alice", "a", "A", time.Second, time.Minute)
	d := selectDevice(t, e, s, "A")
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		release, err := e.Acquire(ctx, s, d, &testCloser{})
		if err == nil {
			release()
		}
		result <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if selectDevice(t, e, s, "A").Queued == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("reader not queued")
		}
		time.Sleep(time.Millisecond)
	}
	e.ReleaseLease(l.ID, "spiffe://test/alice")
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if d := selectDevice(t, e, s, "A"); d.Readers != 0 || d.Queued != 0 {
		t.Fatalf("counters leaked: %+v", d)
	}
}
func TestHotplugOnlyInvalidatesTarget(t *testing.T) {
	e := engineFixture(t)
	a := Scope{Group: "a", Identity: "spiffe://test/alice"}
	b := Scope{Group: "b", Identity: "spiffe://test/bob"}
	ca, cb, cc := &testCloser{}, &testCloser{}, &testCloser{}
	ra := getRead(t, e, a, "A", ca)
	defer ra()
	rb := getRead(t, e, a, "B", cb)
	defer rb()
	rc := getRead(t, e, b, "C", cc)
	defer rc()
	old := selectDevice(t, e, a, "A")
	l, _ := e.CreateLease("spiffe://test/alice", "a", "A", time.Second, time.Minute)
	e.Update(testDevices()[1:])
	waitFor(t, ca.closed.Load)
	if !ca.closed.Load() || cb.closed.Load() || cc.closed.Load() {
		t.Fatal("hotplug affected unrelated device")
	}
	if lease, _ := e.Lease(l.ID); lease.State != "invalidated" {
		t.Fatal("lease survived unplug")
	}
	e.Update(testDevices())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := e.Acquire(ctx, a, old, &testCloser{}); err == nil {
		t.Fatal("old generation accepted")
	}
	r := getRead(t, e, a, "A", &testCloser{})
	r()
}
func TestQueueLimitRevocationAndExpiry(t *testing.T) {
	e := engineFixture(t)
	cfg := testConfig()
	cfg.ReadQueueLimit = 1
	e.Reload(cfg)
	s := Scope{Group: "a", Identity: "spiffe://test/alice"}
	d := selectDevice(t, e, s, "A")
	l, _ := e.CreateLease("spiffe://test/alice", "a", "A", time.Second, 20*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := e.Acquire(ctx, s, d, &testCloser{}); result <- err }()
	deadline := time.Now().Add(time.Second)
	for selectDevice(t, e, s, "A").Queued == 0 {
		if time.Now().After(deadline) {
			t.Fatal("not queued")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := e.Acquire(context.Background(), s, d, &testCloser{}); !errors.Is(err, ErrBusy) {
		t.Fatal("queue limit not enforced")
	}
	cancel()
	<-result
	time.Sleep(25 * time.Millisecond)
	e.mu.Lock()
	e.advance()
	e.signal()
	e.mu.Unlock()
	if e.AuthorizedScope(Scope{Group: "a", Lease: l.ID, Identity: "spiffe://test/alice"}) {
		t.Fatal("expired lease accepted")
	}
	l, _ = e.CreateLease("spiffe://test/alice", "a", "A", time.Second, time.Minute)
	cfg.Identities = map[string][]string{}
	e.Reload(cfg)
	if lease, _ := e.Lease(l.ID); lease.State != "revoked" {
		t.Fatal("removed identity lease not revoked")
	}
}
func TestDuplicateSerialDenied(t *testing.T) {
	e := engineFixture(t)
	ds := testDevices()
	ds = append(ds, Device{Serial: "A", State: "device", TransportID: "9"})
	e.Update(ds)
	if _, err := e.Select(Scope{Group: "a", Identity: "spiffe://test/alice"}, "A", "", ""); err == nil {
		t.Fatal("duplicate serial accepted")
	}
}

type blockingCloser struct{ entered, unblock chan struct{} }

func (c *blockingCloser) Close() error { close(c.entered); <-c.unblock; return nil }
func TestClosingWriterDoesNotBlockOtherDevicesOrGrantSuccessorEarly(t *testing.T) {
	e := engineFixture(t)
	first, err := e.CreateLease("spiffe://test/alice", "a", "A", time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	closer := &blockingCloser{make(chan struct{}), make(chan struct{})}
	release := getRead(t, e, Scope{Group: "a", Lease: first.ID, Identity: "spiffe://test/alice"}, "A", closer)
	next, err := e.CreateLease("spiffe://test/bob", "b", "A", time.Minute, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	e.ReleaseLease(first.ID, "spiffe://test/alice")
	<-closer.entered
	unrelated := getRead(t, e, Scope{Group: "a", Identity: "spiffe://test/alice"}, "B", &testCloser{})
	unrelated()
	if l, _ := e.Lease(next.ID); l.State != "queued" {
		t.Fatal("successor granted before transport closed")
	}
	close(closer.unblock)
	release()
	waitFor(t, func() bool { l, _ := e.Lease(next.ID); return l.State == "active" })
}

func TestIdentityRevocationClosesOrdinaryStreams(t *testing.T) {
	e := engineFixture(t)
	closed := &testCloser{}
	release := getRead(t, e, Scope{Group: "a", Identity: "spiffe://test/alice", Tunnel: "alice"}, "A", closed)
	defer release()
	cfg := testConfig()
	delete(cfg.Identities, "spiffe://test/alice")
	e.Reload(cfg)
	waitFor(t, closed.closed.Load)
	if e.AuthorizedScope(Scope{Group: "a", Identity: "spiffe://test/alice"}) {
		t.Fatal("revoked identity retained access")
	}
}
