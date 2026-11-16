package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const autoOwner = "spiffe://test/auto"

func autoConfig() Config {
	c := testConfig()
	c.AutoGroups = AutoGroupsConfig{
		Configured: SummaryGroupConfig{Enabled: true, Name: "static-devices"},
		Discovery:  SummaryGroupConfig{Enabled: true, Name: "discovered-devices"},
		All:        SummaryGroupConfig{Enabled: true, Name: "all-devices"},
		Serial:     SerialGroupConfig{Enabled: true, Source: "all-devices", InheritAccess: true},
		Model:      ModelGroupConfig{Enabled: true, Source: "all-devices"},
	}
	c.Identities[autoOwner] = []string{"*"}
	c.Defaults()
	return c
}

func assertMembers(t *testing.T, e *Engine, name string, want ...string) {
	t.Helper()
	g, ok := e.Config().Group(name)
	if !ok || !slices.Equal(g.Devices, want) {
		t.Fatalf("group %q = %+v, exists=%v; want %v", name, g, ok, want)
	}
}

func TestAutoGroupsInventoryLifecycle(t *testing.T) {
	c := autoConfig()
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	e := NewEngine(c)
	e.Update(ParseDevices("D unauthorized usb:4 model:Pixel_8 transport_id:4\nA device usb:1 model:Pixel_8 transport_id:1\nB offline usb:2 transport_id:2\n"))
	assertMembers(t, e, "static-devices", "A", "B", "C")
	assertMembers(t, e, "discovered-devices", "D")
	assertMembers(t, e, "all-devices", "A", "B", "C", "D")
	assertMembers(t, e, "model-Pixel_8", "A", "D")
	assertMembers(t, e, "D", "D")
	var names []string
	for _, g := range e.Config().Groups {
		names = append(names, g.Name)
	}
	if want := []string{"a", "b", "static-devices", "discovered-devices", "all-devices", "A", "B", "C", "D", "model-Pixel_8"}; !slices.Equal(names, want) {
		t.Fatalf("group order = %v; want %v", names, want)
	}
	e.Update(nil)
	assertMembers(t, e, "D", "D")
	assertMembers(t, e, "model-Pixel_8", "A", "D")
	_, devices, ok := e.GroupInventory(autoOwner, "D")
	if !ok || len(devices) != 1 || devices[0].State != "disconnected" {
		t.Fatalf("remembered inventory = %+v, %v", devices, ok)
	}
	e.Update(ParseDevices("D device usb:4 transport_id:4\n"))
	assertMembers(t, e, "model-Pixel_8", "A", "D")
	e.Update(ParseDevices("D device usb:4 model:Pixel_9 transport_id:4\n"))
	assertMembers(t, e, "model-Pixel_8", "A")
	assertMembers(t, e, "model-Pixel_9", "D")
	restarted := NewEngine(c)
	assertMembers(t, restarted, "discovered-devices")
	if _, ok := restarted.Config().Group("D"); ok {
		t.Fatal("dynamic inventory survived process recreation")
	}
}

func TestAutoGroupsIndependentSources(t *testing.T) {
	for _, mode := range []string{"all-only", "serial", "model", "discovery-only"} {
		t.Run(mode, func(t *testing.T) {
			c := testConfig()
			switch mode {
			case "all-only":
				c.AutoGroups.All.Enabled = true
			case "serial":
				c.AutoGroups.Serial = SerialGroupConfig{Enabled: true, Source: "a"}
			case "model":
				c.AutoGroups.Model = ModelGroupConfig{Enabled: true, Source: "a"}
			case "discovery-only":
				c.AutoGroups.Discovery.Enabled = true
			}
			c.Defaults()
			if err := c.Validate(); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(c)
			e.Update(ParseDevices("A device usb:1 model:Pixel transport_id:1\nD device usb:2 model:Pixel transport_id:2\n"))
			switch mode {
			case "all-only":
				assertMembers(t, e, "all", "A", "B", "C")
			case "serial":
				assertMembers(t, e, "A", "A")
			case "model":
				assertMembers(t, e, "model-Pixel", "A")
			case "discovery-only":
				assertMembers(t, e, "discovery", "D")
			}
			if _, ok := e.Config().Group("D"); ok {
				t.Fatal("unrequested device group generated")
			}
		})
	}
}

func TestAutoGroupValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Config)
	}{
		{"missing source", func(c *Config) { c.AutoGroups.Serial.Source = "" }},
		{"disabled source", func(c *Config) { c.AutoGroups.All.Enabled = false }},
		{"generated source", func(c *Config) { c.AutoGroups.Model.Source = "A" }},
		{"duplicate summary", func(c *Config) { c.AutoGroups.All.Name = "static-devices" }},
		{"static summary collision", func(c *Config) { c.AutoGroups.All.Name = "a" }},
		{"invalid summary", func(c *Config) { c.AutoGroups.All.Name = "ALL" }},
		{"static serial collision", func(c *Config) { c.Groups[0].Devices = append(c.Groups[0].Devices, "a") }},
		{"summary serial collision", func(c *Config) { c.Groups[0].Devices = append(c.Groups[0].Devices, "all-devices") }},
		{"invalid prefix", func(c *Config) { c.AutoGroups.Model.Prefix = "*" }},
		{"partial wildcard", func(c *Config) { c.Identities[autoOwner] = []string{"*", "model-*"} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := autoConfig()
			tc.edit(&c)
			if err := c.Validate(); err == nil {
				t.Fatal("invalid auto group configuration accepted")
			}
		})
	}
	c := testConfig()
	c.Identities[autoOwner] = []string{"*", "a"}
	if err := c.Validate(); err != nil {
		t.Fatalf("wildcard without auto groups: %v", err)
	}
	c.Identities[autoOwner] = []string{"*", "missing"}
	if c.Validate() == nil {
		t.Fatal("wildcard masked invalid explicit grant")
	}
	c = autoConfig()
	c.AutoGroups.Serial.Enabled = false
	c.Identities[autoOwner] = []string{"model-Future"}
	if err := c.Validate(); err != nil {
		t.Fatalf("model preauthorization rejected: %v", err)
	}
	c.Identities[autoOwner] = []string{"FutureSerial"}
	if c.Validate() == nil {
		t.Fatal("serial preauthorization without serial rule accepted")
	}
}

func TestAutoGroupInheritanceAndWildcard(t *testing.T) {
	c := autoConfig()
	c.Identities["spiffe://test/model"] = []string{"model-Pixel_8"}
	c.Identities["spiffe://test/single"] = []string{"A"}
	c.Identities["spiffe://test/discovery"] = []string{"discovered-devices"}
	c.Identities["spiffe://test/future"] = []string{"Future"}
	e := NewEngine(c)
	if e.AuthorizedScope(Scope{Identity: autoOwner, Group: "missing"}) || e.AuthorizedScope(Scope{Identity: "spiffe://test/future", Group: "Future"}) {
		t.Fatal("authorization created a nonexistent group")
	}
	e.Update(ParseDevices("A device usb:1 model:Pixel_8 transport_id:1\nD device usb:4 model:Pixel_8 transport_id:4\nFuture device usb:5 transport_id:5\n"))
	for _, tc := range []struct {
		identity, group string
		want            bool
	}{
		{"alice", "A", true}, {"alice", "C", false}, {"alice", "model-Pixel_8", false},
		{"model", "A", true}, {"model", "D", true}, {"model", "B", false},
		{"single", "A", true}, {"single", "model-Pixel_8", false}, {"single", "a", false},
		{"discovery", "D", true}, {"discovery", "A", false}, {"future", "Future", true},
		{"unknown", "all-devices", false}, {"auto", "model-Pixel_8", true}, {"auto", "Future", true},
	} {
		if got := e.Config().Allows("spiffe://test/"+tc.identity, tc.group); got != tc.want {
			t.Errorf("Allows(%s, %s) = %v; want %v", tc.identity, tc.group, got, tc.want)
		}
	}
	c.AutoGroups.Serial.InheritAccess = false
	if err := e.Reload(c); err != nil {
		t.Fatal(err)
	}
	if e.Config().Allows("spiffe://test/model", "A") || !e.Config().Allows("spiffe://test/single", "A") {
		t.Fatal("explicit serial authorization did not remain independent of inheritance")
	}
}

func TestAutoGroupRuntimeCollisionsAndReload(t *testing.T) {
	c := autoConfig()
	e := NewEngine(c)
	e.Update(ParseDevices("a device usb:1 transport_id:1\nA device usb:2 model:Pixel_8 transport_id:2\nmodel-Pixel_8 device usb:3 transport_id:3\n"))
	assertMembers(t, e, "a", "A", "B")
	if _, ok := e.Config().Group("model-Pixel_8"); ok {
		t.Fatal("colliding generated groups were published")
	}
	if err := e.Reload(c); err == nil {
		t.Fatal("reload with known conflicts accepted")
	}
	assertMembers(t, e, "a", "A", "B")

	e = NewEngine(c)
	e.Update(ParseDevices("A device usb:1 transport_id:1\nD device usb:4 transport_id:4\n"))
	c.Groups[0].Devices = []string{"B", "D"}
	if err := e.Reload(c); err != nil {
		t.Fatal(err)
	}
	assertMembers(t, e, "static-devices", "A", "B", "C", "D") // A is still in b.
	assertMembers(t, e, "discovered-devices")
	c.Groups[1].Devices = []string{"C"}
	if err := e.Reload(c); err != nil {
		t.Fatal(err)
	}
	assertMembers(t, e, "discovered-devices", "A")
	if autoConfig().ReloadCompatible(c) == false {
		t.Fatal("membership-only reload requires restart")
	}
	c.AutoGroups.All.Name = "renamed"
	if autoConfig().ReloadCompatible(c) {
		t.Fatal("auto group settings accepted without restart")
	}
}

func TestAutoGroupReloadAdmitsAlreadyConnectedStaticDevice(t *testing.T) {
	c := testConfig()
	c.AutoGroups.Model = ModelGroupConfig{Enabled: true, Source: "a"}
	e := NewEngine(c)
	e.Update(ParseDevices("D device usb:4 model:Pixel_8 transport_id:4\n"))
	if _, ok := e.Config().Group("model-Pixel_8"); ok {
		t.Fatal("unconfigured device admitted without discovery")
	}
	c.Groups[0].Devices = append(c.Groups[0].Devices, "D")
	if err := e.Reload(c); err != nil {
		t.Fatal(err)
	}
	assertMembers(t, e, "model-Pixel_8", "D")
}

func TestAutoGroupRevocationAndSharedLocks(t *testing.T) {
	c := autoConfig()
	c.Identities[autoOwner] = []string{"model-Pixel_8"}
	e := NewEngine(c)
	e.Update(ParseDevices("A device usb:1 model:Pixel_8 transport_id:1\n"))
	scope := Scope{Identity: autoOwner, Group: "A"}
	closer := &testCloser{}
	release := getRead(t, e, scope, "A", closer)
	defer release()
	lease, err := e.CreateLease(autoOwner, "model-Pixel_8", "A", time.Second, time.Minute)
	if err != nil || lease.State != "queued" {
		t.Fatalf("reader in serial group failed to block model writer: %+v %v", lease, err)
	}
	e.Update(ParseDevices("A device usb:1 model:Pixel_9 transport_id:1\n"))
	if lease, _ := e.Lease(lease.ID); lease.State != "revoked" {
		t.Fatalf("model migration did not revoke lease: %+v", lease)
	}
	deadline := time.Now().Add(time.Second)
	for !closer.closed.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !closer.closed.Load() || e.AuthorizedScope(scope) {
		t.Fatal("model migration did not revoke inherited access")
	}
	for {
		e.mu.Lock()
		closing := e.closing["A"]
		e.mu.Unlock()
		if closing == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("revoked transport did not release its lock")
		}
		time.Sleep(time.Millisecond)
	}

	c.Identities[autoOwner] = []string{"*"}
	if err := e.Reload(c); err != nil {
		t.Fatal(err)
	}
	lease, err = e.CreateLease(autoOwner, "A", "A", time.Second, time.Minute)
	if err != nil || lease.State != "active" {
		t.Fatalf("wildcard writer: %+v %v", lease, err)
	}
	if e.AuthorizedScope(Scope{Identity: autoOwner, Group: "A", Lease: "invalid"}) {
		t.Fatal("wildcard bypassed lease validation")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := e.Acquire(ctx, Scope{Identity: autoOwner, Group: "a"}, selectDevice(t, e, scope, "A"), &testCloser{}); err == nil {
		t.Fatal("static reader bypassed serial writer")
	}
	c.Identities[autoOwner] = []string{"b"}
	if err := e.Reload(c); err != nil {
		t.Fatal(err)
	}
	if e.Config().Allows(autoOwner, "model-Pixel_9") {
		t.Fatal("removed wildcard retained access")
	}
	// b still includes A, so the serial lease remains authorized by inheritance.
	if l, _ := e.Lease(lease.ID); l.State != "active" {
		t.Fatal("remaining explicit permissions were ignored")
	}
	c.Identities[autoOwner] = []string{}
	if err := e.Reload(c); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.Lease(lease.ID); l.State != "revoked" {
		t.Fatal("wildcard removal did not revoke unauthorized lease")
	}
}

func TestAutoGroupWaitAndSnapshotIsolation(t *testing.T) {
	c := autoConfig()
	e := NewEngine(c)
	scope := Scope{Identity: autoOwner, Group: "A"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.waitFor(ctx, scope, "A", "", "any", []string{"device"}) }()
	assertDeviceWaiting(t, done)
	e.Update(ParseDevices("A device usb:1 model:Pixel_8 transport_id:1\n"))
	if err := deviceWaitResult(t, done); err != nil {
		t.Fatal(err)
	}
	// Neither caller-owned source data nor a returned snapshot can mutate grants.
	c.Identities[autoOwner][0] = "b"
	snapshot := e.Config()
	snapshot.Identities[autoOwner][0] = "b"
	snapshot.Groups[0].Devices[0] = "changed"
	assertMembers(t, e, "a", "A", "B")
	if !e.Config().Allows(autoOwner, "model-Pixel_8") {
		t.Fatal("configuration aliases changed engine permissions")
	}
}

func TestAutoGroupAPIPathsAndWildcard(t *testing.T) {
	c := autoConfig()
	e := NewEngine(c)
	serial := "9A/B:%?#"
	e.Update([]Device{{Serial: serial, State: "device", TransportID: "9", Model: "Pixel_8"}})
	groupPath := "/v1/groups/" + url.PathEscape(serial)
	devicePath := groupPath + "/devices/" + url.PathEscape(serial)
	for _, path := range []string{groupPath, groupPath + "/devices", devicePath} {
		w := apiRequest(t, e, autoOwner, "GET", path, "")
		if w.Code != http.StatusOK {
			t.Fatalf("escaped auto group %s: %d %s", path, w.Code, w.Body)
		}
	}
	w := apiRequest(t, e, autoOwner, "POST", devicePath+"/write-leases", `{}`)
	if w.Code != http.StatusCreated || !strings.HasPrefix(w.Header().Get("Location"), devicePath+"/write-leases/") {
		t.Fatalf("escaped auto lease: %d %s", w.Code, w.Body)
	}
	if got := apiRequest(t, e, autoOwner, "GET", w.Header().Get("Location"), ""); got.Code != http.StatusOK {
		t.Fatalf("escaped lease location: %d %s", got.Code, got.Body)
	}
	w = apiRequest(t, e, autoOwner, "GET", "/v1/groups", "")
	var groups []Group
	if err := json.Unmarshal(w.Body.Bytes(), &groups); err != nil || !reflect.DeepEqual(groups, e.Config().Groups) {
		t.Fatalf("wildcard list = %s, %v", w.Body, err)
	}
	for _, tc := range []struct {
		identity, path string
		code           int
	}{
		{autoOwner, "/v1/groups/missing", http.StatusNotFound},
		{"spiffe://test/unknown", "/v1/groups", http.StatusForbidden},
	} {
		if got := apiRequest(t, e, tc.identity, "GET", tc.path, ""); got.Code != tc.code {
			t.Fatalf("%s %s = %d; want %d", tc.identity, tc.path, got.Code, tc.code)
		}
	}
}
