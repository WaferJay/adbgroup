package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestAPIGroupDeviceDetail(t *testing.T) {
	e := engineFixture(t)
	e.Update(testDevices()[1:])
	w := apiRequest(t, e, "spiffe://test/alice", "GET", "/v1/groups/a/devices/A", "")
	var detail struct {
		Device
		Group  string       `json:"group"`
		Access deviceAccess `json:"access"`
	}
	if w.Code != http.StatusOK {
		t.Fatalf("device detail: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Serial != "A" || detail.State != "disconnected" || detail.Group != "a" || detail.Access != (deviceAccess{"mtls", "/v1/groups/a/tunnels"}) {
		t.Fatalf("incorrect device detail: %+v", detail)
	}
	for _, path := range []string{"/v1/groups/a/devices/C", "/v1/groups/a/devices/missing", "/v1/groups/b/devices/A", "/v1/groups/missing/devices/A"} {
		w = apiRequest(t, e, "spiffe://test/alice", "GET", path, "")
		if w.Code != http.StatusNotFound || strings.TrimSpace(w.Body.String()) != `{"error":"device not found"}` {
			t.Fatalf("device path disclosed membership: %s: %d %s", path, w.Code, w.Body)
		}
	}
	w = apiRequest(t, e, "spiffe://test/alice", "POST", "/v1/groups/a/devices/A/write-leases", `{}`)
	if w.Code != http.StatusConflict {
		t.Fatalf("offline device leased: %d %s", w.Code, w.Body)
	}
}

func createDeviceLease(t *testing.T, e *Engine, identity, group, serial string) (Lease, string, string) {
	t.Helper()
	path := "/v1/groups/" + group + "/devices/" + serial + "/write-leases"
	w := apiRequest(t, e, identity, "POST", path, `{"wait_seconds":30,"ttl_seconds":300}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("create device lease: %d %s", w.Code, w.Body)
	}
	var result struct {
		Lease  Lease  `json:"lease"`
		Secret string `json:"ssh_password"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	location := w.Header().Get("Location")
	if location != path+"/"+result.Lease.ID || result.Lease.Group != group || result.Lease.Serial != serial || result.Secret != "" {
		t.Fatalf("invalid lease or canonical location: %s %s", location, w.Body)
	}
	return result.Lease, result.Secret, location
}

func TestAPINestedLeaseLifecycle(t *testing.T) {
	e := engineFixture(t)
	owner := "spiffe://test/alice"
	lease, _, path := createDeviceLease(t, e, owner, "a", "A")
	w := apiRequest(t, e, owner, "GET", path, "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), "ssh_password") {
		t.Fatalf("get nested lease: %d %s", w.Code, w.Body)
	}
	expires := time.Now().Add(10 * time.Minute).UTC().Truncate(time.Second)
	body := `{"expires_at":"` + expires.Format(time.RFC3339Nano) + `"}`
	w = apiRequest(t, e, owner, "PATCH", path, body)
	if w.Code != http.StatusOK {
		t.Fatalf("patch lease: %d %s", w.Code, w.Body)
	}
	updated, _ := e.Lease(lease.ID)
	if !updated.Expires.Equal(expires) {
		t.Fatalf("expiry was not set exactly: got %s, want %s", updated.Expires, expires)
	}
	if !e.AuthorizedScope(Scope{Group: "a", Lease: lease.ID, Identity: owner}) {
		t.Fatal("nested lease not usable with mTLS")
	}
	for i := 0; i < 2; i++ {
		w = apiRequest(t, e, owner, "DELETE", path, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("nested delete not idempotent: %d %s", w.Code, w.Body)
		}
	}
	if e.AuthorizedScope(Scope{Group: "a", Lease: lease.ID, Identity: owner}) {
		t.Fatal("released nested lease usable with mTLS")
	}
	w = apiRequest(t, e, owner, "PATCH", path, body)
	if w.Code != http.StatusConflict {
		t.Fatalf("terminal lease renewed: %d %s", w.Code, w.Body)
	}
}

func TestAPINestedLeaseRejectsForgedScope(t *testing.T) {
	e := engineFixture(t)
	cfg := testConfig()
	cfg.Identities["spiffe://test/alice"] = []string{"a", "b"}
	cfg.Identities["spiffe://test/bob"] = []string{"a", "b"}
	e.Reload(cfg)
	owner := "spiffe://test/alice"
	lease, _, path := createDeviceLease(t, e, owner, "a", "A")
	wrongPaths := []string{
		"/v1/groups/a/devices/B/write-leases/" + lease.ID,
		"/v1/groups/b/devices/A/write-leases/" + lease.ID,
		"/v1/groups/missing/devices/A/write-leases/" + lease.ID,
		path + "-missing",
	}
	for _, wrong := range wrongPaths {
		for _, method := range []string{"GET", "PATCH", "DELETE"} {
			w := apiRequest(t, e, owner, method, wrong, `{"expires_at":"2030-01-01T00:00:00Z"}`)
			if w.Code != http.StatusNotFound {
				t.Fatalf("scope bypass: %s %s: %d %s", method, wrong, w.Code, w.Body)
			}
		}
	}
	for _, method := range []string{"GET", "PATCH", "DELETE"} {
		w := apiRequest(t, e, "spiffe://test/bob", method, path, `{}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("other lease owner accepted: %s %d %s", method, w.Code, w.Body)
		}
	}
	if !e.AuthorizedScope(Scope{Group: "a", Lease: lease.ID, Identity: owner}) {
		t.Fatal("forged requests changed the real lease")
	}
}

func TestAPINestedLeaseValidation(t *testing.T) {
	e := engineFixture(t)
	owner := "spiffe://test/alice"
	collection := "/v1/groups/a/devices/A/write-leases"
	for _, body := range []string{`{"group":"b"}`, `{"serial":"B"}`, `{"wait_seconds":-1}`, `{"ttl_seconds":1801}`, `{} {}`} {
		w := apiRequest(t, e, owner, "POST", collection, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid create accepted: %s: %d %s", body, w.Code, w.Body)
		}
	}
	_, _, path := createDeviceLease(t, e, owner, "a", "A")
	for _, body := range []string{`{}`, `{"expires_at":null}`, `{"expires_at":"invalid"}`, `{"expires_at":"2000-01-01T00:00:00Z"}`, `{"expires_at":"` + time.Now().Add(31*time.Minute).Format(time.RFC3339Nano) + `"}`, `{"ttl_seconds":60}`, `{"expires_at":"` + time.Now().Add(time.Minute).Format(time.RFC3339Nano) + `"} {}`, `{"unknown":1}`} {
		w := apiRequest(t, e, owner, "PATCH", path, body)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("invalid patch accepted: %s: %d %s", body, w.Code, w.Body)
		}
	}
	for _, req := range []struct{ method, path, body string }{
		{"GET", "/v1/groups/a/devices/A", ""},
		{"POST", collection, `{}`},
		{"GET", path, ""},
		{"PATCH", path, `{}`},
		{"DELETE", path, ""},
	} {
		w := apiRequest(t, e, "", req.method, req.path, req.body)
		if w.Code != http.StatusForbidden {
			t.Fatalf("missing identity accepted: %s %s: %d", req.method, req.path, w.Code)
		}
	}
	for _, group := range []string{"b", "missing"} {
		w := apiRequest(t, e, owner, "POST", "/v1/groups/"+group+"/devices/A/write-leases", `{}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("inaccessible device leased: %d %s", w.Code, w.Body)
		}
	}
}

func TestAPINestedLeasesSharePhysicalDeviceLock(t *testing.T) {
	e := engineFixture(t)
	read := getRead(t, e, Scope{Group: "a", Identity: "spiffe://test/alice"}, "A", &testCloser{})
	first, _, firstPath := createDeviceLease(t, e, "spiffe://test/alice", "a", "A")
	second, _, secondPath := createDeviceLease(t, e, "spiffe://test/bob", "b", "A")
	if first.State != "queued" || second.State != "queued" {
		t.Fatal("nested leases bypassed existing reader")
	}
	patch := apiRequest(t, e, "spiffe://test/alice", "PATCH", firstPath, `{"expires_at":"`+time.Now().Add(time.Minute).Format(time.RFC3339Nano)+`"}`)
	if patch.Code != http.StatusConflict {
		t.Fatalf("queued lease expiry modified: %d %s", patch.Code, patch.Body)
	}
	read()
	if l, _ := e.Lease(first.ID); l.State != "active" {
		t.Fatal("first group writer not granted")
	}
	if l, _ := e.Lease(second.ID); l.State != "queued" {
		t.Fatal("shared device has two active writers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := e.Acquire(ctx, Scope{Group: "b", Identity: "spiffe://test/bob"}, selectDevice(t, e, Scope{Group: "b", Identity: "spiffe://test/bob"}, "A"), &testCloser{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("other group's reader bypassed global lock: %v", err)
	}
	w := apiRequest(t, e, "spiffe://test/alice", "DELETE", firstPath, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("release: %d %s", w.Code, w.Body)
	}
	if l, _ := e.Lease(second.ID); l.State != "active" {
		t.Fatal("second group writer not granted after release")
	}
	w = apiRequest(t, e, "spiffe://test/bob", "DELETE", secondPath, "")
	if w.Code != http.StatusNoContent {
		t.Fatalf("release second: %d %s", w.Code, w.Body)
	}
}

func TestAPINestedLeaseLocationEscapesDeviceSerial(t *testing.T) {
	e := engineFixture(t)
	cfg := testConfig()
	cfg.Groups[0].Devices = []string{"A/B"}
	e.Reload(cfg)
	e.Update([]Device{{Serial: "A/B", State: "device", TransportID: "9"}})
	w := apiRequest(t, e, "spiffe://test/alice", "POST", "/v1/groups/a/devices/A%2FB/write-leases", `{}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("encoded device rejected: %d %s", w.Code, w.Body)
	}
	location := w.Header().Get("Location")
	if !strings.HasPrefix(location, "/v1/groups/a/devices/A%2FB/write-leases/") {
		t.Fatalf("serial not escaped in Location: %q", location)
	}
	w = apiRequest(t, e, "spiffe://test/alice", "GET", location, "")
	if w.Code != http.StatusOK {
		t.Fatalf("canonical Location cannot be queried: %d %s", w.Code, w.Body)
	}
}

func TestRemovedAPIEndpoints(t *testing.T) {
	e := engineFixture(t)
	owner := "spiffe://test/alice"
	lease, _, _ := createDeviceLease(t, e, owner, "a", "A")
	for _, request := range []struct{ method, path string }{
		{"GET", "/v1/devices"},
		{"POST", "/v1/write-leases"},
		{"GET", "/v1/write-leases/" + lease.ID},
		{"POST", "/v1/write-leases/" + lease.ID + "/renew"},
		{"DELETE", "/v1/write-leases/" + lease.ID},
	} {
		w := apiRequest(t, e, owner, request.method, request.path, `{}`)
		if w.Code != http.StatusNotFound {
			t.Fatalf("removed endpoint still available: %s %s: %d %s", request.method, request.path, w.Code, w.Body)
		}
	}
	if !e.AuthorizedScope(Scope{Group: "a", Lease: lease.ID, Identity: owner}) {
		t.Fatal("removed endpoint changed the active lease")
	}
}
