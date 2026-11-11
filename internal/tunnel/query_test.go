package tunnel

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

func TestInventoryQueriesAuthorizeAndDoNotCreateTunnels(t *testing.T) {
	h := newHarness(t)
	defer h.client.CloseIdleConnections()
	cfg := h.engine.Config()
	cfg.Groups = append(cfg.Groups, gateway.Group{Name: "hidden", Devices: []string{"SECRET"}})
	h.engine.Reload(cfg)
	h.engine.Update([]gateway.Device{{Serial: "A", State: "device", TransportID: "1"}})
	groups, err := h.client.ListGroups(h.server.ctx)
	if err != nil || len(groups) != 1 || groups[0].Name != "a" {
		t.Fatalf("authorized groups = %+v, %v", groups, err)
	}
	devices, err := h.client.ListDevices(h.server.ctx, "")
	if err != nil || len(devices) != 2 || devices[0].Serial != "A" || devices[0].State != "device" || devices[1].Serial != "B" || devices[1].State != "disconnected" {
		t.Fatalf("default group devices = %+v, %v", devices, err)
	}
	if _, err = h.client.ListDevices(h.server.ctx, "hidden"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("unauthorized group not rejected: %v", err)
	}
	h.server.mu.Lock()
	sessions := len(h.server.sessions)
	h.server.mu.Unlock()
	h.local.mu.Lock()
	listeners := len(h.local.listeners)
	h.local.mu.Unlock()
	if sessions != 0 || listeners != 0 {
		t.Fatalf("inventory query created %d tunnels and %d listeners", sessions, listeners)
	}
}

func TestInventoryQueriesRequireExplicitGroupWhenAmbiguous(t *testing.T) {
	h := newHarness(t)
	defer h.client.CloseIdleConnections()
	cfg := h.engine.Config()
	cfg.Groups = append(cfg.Groups, gateway.Group{Name: "b", Devices: []string{}})
	cfg.Identities = map[string][]string{"spiffe://adbgroup/alice": {"a", "b"}}
	h.engine.Reload(cfg)
	if _, err := h.client.ListDevices(h.server.ctx, ""); err == nil || !strings.Contains(err.Error(), "multiple groups authorized (a, b)") {
		t.Fatalf("ambiguous group not rejected: %v", err)
	}
	devices, err := h.client.ListDevices(h.server.ctx, "b")
	if err != nil || devices == nil || len(devices) != 0 {
		t.Fatalf("explicit empty group devices = %+v, %v", devices, err)
	}
	ctx, cancel := context.WithCancel(h.server.ctx)
	cancel()
	if _, err = h.client.ListGroups(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query error = %v", err)
	}
}
