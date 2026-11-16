package tunnel

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

func TestMTLSAutoGroupTunnelAndWildcardRevocation(t *testing.T) {
	h := newHarness(t)
	defer h.client.CloseIdleConnections()
	cfg := h.engine.Config()
	cfg.AutoGroups = gateway.AutoGroupsConfig{
		All:       gateway.SummaryGroupConfig{Enabled: true, Name: "everything"},
		Discovery: gateway.SummaryGroupConfig{Enabled: true, Name: "found"},
		Serial:    gateway.SerialGroupConfig{Enabled: true, Source: "everything", InheritAccess: true},
		Model:     gateway.ModelGroupConfig{Enabled: true, Source: "everything", Prefix: "model-"},
	}
	owner := "spiffe://adbgroup/alice"
	cfg.Identities[owner] = []string{"*"}
	if err := h.engine.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	const serial = "9A/B"
	h.engine.Update([]gateway.Device{{Serial: serial, State: "device", TransportID: "9", Model: "Pixel_8"}})
	groups, err := h.client.ListGroups(h.server.ctx)
	if err != nil || len(groups) != 7 { // a, found, everything, A, B, 9A/B, model-Pixel_8
		t.Fatalf("wildcard groups = %+v, %v", groups, err)
	}
	if _, err := h.client.resolveGroup(h.server.ctx, ""); err == nil || !strings.Contains(err.Error(), "multiple groups authorized") {
		t.Fatalf("wildcard ambiguities were not rejected: %v", err)
	}
	var resource Resource
	if err := h.client.request(h.server.ctx, "POST", "/v1/groups/missing/tunnels", map[string]string{}, &resource); err == nil {
		t.Fatal("wildcard created a tunnel for nonexistent group")
	}
	if err := h.client.request(h.server.ctx, "POST", "/v1/groups/"+url.PathEscape(serial)+"/tunnels", map[string]string{"lease_id": "invalid"}, &resource); err == nil {
		t.Fatal("wildcard bypassed active lease checks")
	}
	ctx, cancel := context.WithCancel(h.server.ctx)
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- h.client.RunReady(ctx, serial, "", "127.0.0.1:15037", func(group, address string) { ready <- group })
	}()
	select {
	case group := <-ready:
		if group != serial {
			t.Fatalf("ready group = %q", group)
		}
	case err := <-done:
		t.Fatalf("auto serial tunnel: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("auto serial tunnel did not become ready")
	}
	conn := h.adbDevice(t, serial, "tcp:80")
	defer conn.Close()
	echo(t, conn)
	cfg.Identities[owner] = []string{"a"}
	if err := h.engine.Reload(cfg); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("dynamic serial stream survived wildcard revocation")
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("auto serial tunnel survived wildcard revocation")
	}
}
