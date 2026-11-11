package gateway

import (
	"context"
	"io"
	"net"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func startDeviceWait(t *testing.T, g *Gateway, s Scope, req string) (net.Conn, <-chan error, <-chan struct{}) {
	t.Helper()
	c, server := net.Pipe()
	done := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	t.Cleanup(func() { c.Close() })
	go func() { g.Handle(ctx, server, s); close(done) }()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if err := WriteFrame(c, req); err != nil {
		t.Fatal(err)
	}
	if err := readStatus(c); err != nil {
		t.Fatalf("wait request rejected: %v", err)
	}
	result := make(chan error, 1)
	go func() { result <- readStatus(c) }()
	return c, result, done
}

func assertDeviceWaiting(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("wait completed before its condition: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
}

func deviceWaitResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(time.Second):
		t.Fatal("wait did not complete")
		return nil
	}
}

func TestWaitForDeviceArrivalAndAuthorization(t *testing.T) {
	g, _ := gatewayFixture(t)
	g.Engine.Update(nil)
	c, result, _ := startDeviceWait(t, g, scopeA(), "host-serial:A:wait-for-any-device")
	assertDeviceWaiting(t, result)
	// Neither another serial nor an unauthorized target state completes the wait.
	g.Engine.Update([]Device{{Serial: "B", State: "device", TransportID: "2"}, {Serial: "C", State: "device", TransportID: "3"}})
	assertDeviceWaiting(t, result)
	g.Engine.Update([]Device{{Serial: "A", State: "unauthorized", TransportID: "1"}})
	assertDeviceWaiting(t, result)
	g.Engine.Update([]Device{{Serial: "A", State: "offline", TransportID: "1"}})
	assertDeviceWaiting(t, result)
	g.Engine.Update([]Device{{Serial: "A", State: "device", TransportID: "4"}})
	if err := deviceWaitResult(t, result); err != nil {
		t.Fatal(err)
	}
	var b [1]byte
	if _, err := c.Read(b[:]); err != io.EOF {
		t.Fatalf("expected connection to close after completion, got %v", err)
	}
}

func TestWaitForDeviceSelectorsAndStates(t *testing.T) {
	for _, req := range []string{
		"host:wait-for-any-device",
		"host-usb:wait-for-usb-device",
		"host-serial:A:wait-for-any-device",
		"host-transport-id:1:wait-for-any-device",
	} {
		t.Run(req, func(t *testing.T) {
			g, _ := gatewayFixture(t)
			g.Engine.Update(testDevices()[:1])
			_, result, _ := startDeviceWait(t, g, scopeA(), req)
			if err := deviceWaitResult(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, state := range []string{"recovery", "rescue", "sideload", "bootloader", "any", "device-recovery"} {
		t.Run(state, func(t *testing.T) {
			g, _ := gatewayFixture(t)
			initial := "offline"
			if state == "any" {
				g.Engine.Update(nil)
			} else {
				g.Engine.Update([]Device{{Serial: "A", State: initial, TransportID: "1"}})
			}
			_, result, _ := startDeviceWait(t, g, scopeA(), "host-serial:A:wait-for-any-"+state)
			assertDeviceWaiting(t, result)
			target := state
			if state == "any" {
				target = "offline"
			} else if state == "device-recovery" {
				target = "recovery"
			}
			g.Engine.Update([]Device{{Serial: "A", State: target, TransportID: "1"}})
			if err := deviceWaitResult(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWaitForDisconnect(t *testing.T) {
	for _, selector := range []string{"host-serial:A:", "host-transport-id:1:"} {
		t.Run(selector, func(t *testing.T) {
			g, _ := gatewayFixture(t)
			_, result, _ := startDeviceWait(t, g, scopeA(), selector+"wait-for-any-disconnect")
			assertDeviceWaiting(t, result)
			devices := testDevices()
			devices[0].State = "offline"
			g.Engine.Update(devices)
			assertDeviceWaiting(t, result)
			g.Engine.Update(devices[1:])
			if err := deviceWaitResult(t, result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestWaitForDeviceScopeIsolationAndDisconnect(t *testing.T) {
	g, _ := gatewayFixture(t)
	g.Engine.Update(testDevices()[2:])
	_, result, _ := startDeviceWait(t, g, scopeA(), "host:wait-for-any-device")
	assertDeviceWaiting(t, result)
	g.Engine.Update(testDevices()[:1])
	if err := deviceWaitResult(t, result); err != nil {
		t.Fatal(err)
	}
	_, result, _ = startDeviceWait(t, g, scopeA(), "host-serial:B:wait-for-any-disconnect")
	if err := deviceWaitResult(t, result); err != nil {
		t.Fatalf("absent authorized device should already be disconnected: %v", err)
	}
	_, result, _ = startDeviceWait(t, g, scopeA(), "host-serial:C:wait-for-any-disconnect")
	if err := deviceWaitResult(t, result); err == nil {
		t.Fatal("out-of-group serial was accepted")
	}
}

func TestWaitForDeviceAmbiguity(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		g, _ := gatewayFixture(t)
		if duplicate {
			g.Engine.Update([]Device{{Serial: "A", State: "device", TransportID: "1"}, {Serial: "A", State: "device", TransportID: "2"}})
		}
		_, result, _ := startDeviceWait(t, g, scopeA(), "host:wait-for-any-device")
		if err := deviceWaitResult(t, result); err == nil || !strings.Contains(err.Error(), "more than one") {
			t.Fatalf("ambiguous devices should fail: %v", err)
		}
	}
}

func TestWaitForDeviceDoesNotAcquireLock(t *testing.T) {
	g, _ := gatewayFixture(t)
	lease, err := g.Engine.CreateLease(scopeA().Identity, "a", "A", time.Second, time.Minute)
	if err != nil || lease.State != "active" {
		t.Fatalf("create writer: %v %+v", err, lease)
	}
	_, result, _ := startDeviceWait(t, g, scopeA(), "host-serial:A:wait-for-any-device")
	if err := deviceWaitResult(t, result); err != nil {
		t.Fatal(err)
	}
	d := selectDevice(t, g.Engine, scopeA(), "A")
	if d.Readers != 0 || d.Queued != 0 || d.Writer != lease.ID {
		t.Fatalf("wait changed device locks: %+v", d)
	}
}

func TestWaitForDeviceCancellationAndRevocation(t *testing.T) {
	for _, action := range []string{"disconnect", "revoke-identity", "remove-member", "release-lease", "local-transport"} {
		t.Run(action, func(t *testing.T) {
			g, _ := gatewayFixture(t)
			s := scopeA()
			req := "host-serial:A:wait-for-any-recovery"
			if action == "release-lease" {
				lease, err := g.Engine.CreateLease(s.Identity, "a", "A", time.Second, time.Minute)
				if err != nil {
					t.Fatal(err)
				}
				s.Lease = lease.ID
			} else if action == "local-transport" {
				req = "host-local:wait-for-local-device"
			}
			c, result, done := startDeviceWait(t, g, s, req)
			assertDeviceWaiting(t, result)
			switch action {
			case "disconnect", "local-transport":
				c.Close()
			case "release-lease":
				if err := g.Engine.ReleaseLease(s.Lease, s.Identity); err != nil {
					t.Fatal(err)
				}
			default:
				cfg := testConfig()
				if action == "revoke-identity" {
					delete(cfg.Identities, s.Identity)
				} else {
					cfg.Groups[0].Devices = []string{"B"}
				}
				g.Engine.Reload(cfg)
			}
			if err := deviceWaitResult(t, result); err == nil {
				t.Fatal("cancelled or revoked wait succeeded")
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				t.Fatal("waiting handler leaked")
			}
		})
	}
}

func TestWaitForDeviceRejectsInvalidCommands(t *testing.T) {
	for _, cmd := range []string{"wait-for-any", "wait-for-any-", "wait-for-network-device", "wait-for-any-offline", "wait-for-any-device-invalid"} {
		g, _ := gatewayFixture(t)
		c := request(t, g, "host-serial:A:"+cmd)
		if err := readStatus(c); err == nil {
			t.Fatalf("invalid wait command accepted: %s", cmd)
		}
		c.Close()
	}
}

func TestNativeADBWaitForDevice(t *testing.T) {
	adb, err := exec.LookPath("adb")
	if err != nil {
		t.Skip("adb binary not installed")
	}
	for _, tc := range []struct {
		name    string
		args    []string
		initial []Device
		target  []Device
	}{
		{"arrival", []string{"wait-for-device"}, nil, testDevices()[:1]},
		{"usb", []string{"-d", "wait-for-device"}, nil, testDevices()[:1]},
		{"serial", []string{"-s", "A", "wait-for-device"}, nil, testDevices()[:1]},
		{"transport-id", []string{"-t", "1", "wait-for-device"}, nil, testDevices()[:1]},
		{"recovery", []string{"-s", "A", "wait-for-recovery"}, testDevices()[:1], []Device{{Serial: "A", State: "recovery", TransportID: "1"}}},
		{"disconnect", []string{"-s", "A", "wait-for-disconnect"}, testDevices()[:1], nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, _ := gatewayFixture(t)
			g.Engine.Update(tc.initial)
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Skipf("TCP listeners unavailable: %v", err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			go g.Serve(ctx, listener, scopeA())
			host, port, _ := net.SplitHostPort(listener.Addr().String())
			cmd := exec.CommandContext(ctx, adb, append([]string{"-H", host, "-P", port}, tc.args...)...)
			result := make(chan error, 1)
			var out []byte
			go func() {
				var err error
				out, err = cmd.CombinedOutput()
				result <- err
			}()
			assertDeviceWaiting(t, result)
			g.Engine.Update(tc.target)
			if err := deviceWaitResult(t, result); err != nil {
				t.Fatalf("adb %v: %v\n%s", tc.args, err, out)
			}
		})
	}
}
