package gateway

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func apiRequest(t *testing.T, e *Engine, identity, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if identity != "" {
		u, _ := url.Parse(identity)
		cert := &x509.Certificate{URIs: []*url.URL{u}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	}
	w := httptest.NewRecorder()
	(API{Engine: e}).Handler().ServeHTTP(w, r)
	return w
}
func TestAPIDeviceInventoryKeepsDisconnectedMembership(t *testing.T) {
	e := engineFixture(t)
	e.Update(testDevices()[1:])
	w := apiRequest(t, e, "spiffe://test/alice", "GET", "/v1/groups/a/devices", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "disconnected") || strings.Contains(w.Body.String(), `"serial":"C"`) {
		t.Fatalf("bad inventory: %s", w.Body)
	}
}
func TestWriterWaitTimeoutLetsReadersResume(t *testing.T) {
	e := engineFixture(t)
	s := Scope{Group: "a", Identity: "spiffe://test/alice"}
	release := getRead(t, e, s, "A", &testCloser{})
	defer release()
	l, _ := e.CreateLease("spiffe://test/alice", "a", "A", time.Millisecond, time.Minute)
	time.Sleep(2 * time.Millisecond)
	e.mu.Lock()
	e.advance()
	e.signal()
	e.mu.Unlock()
	if lease, _ := e.Lease(l.ID); lease.State != "timeout" {
		t.Fatal("writer did not time out")
	}
	r := getRead(t, e, s, "A", &testCloser{})
	r()
}

func TestAPIGroupResources(t *testing.T) {
	e := engineFixture(t)
	cfg := testConfig()
	cfg.Groups = append(cfg.Groups, Group{Name: "empty"})
	cfg.Identities["spiffe://test/alice"] = []string{"a", "empty"}
	e.Reload(cfg)
	e.Update(testDevices()[1:]) // A is disconnected but remains assigned to a and b.
	lease, err := e.CreateLease("spiffe://test/alice", "a", "B", time.Second, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	owner := "spiffe://test/alice"
	w := apiRequest(t, e, owner, "GET", "/v1/groups", "")
	var groups []Group
	if w.Code != http.StatusOK {
		t.Fatalf("list groups: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 || !reflect.DeepEqual(groups[0], cfg.Groups[0]) || groups[1].Name != "empty" || groups[1].Devices == nil {
		t.Fatalf("unexpected groups: %+v", groups)
	}
	for _, key := range []string{`"name"`, `"devices"`} {
		if !strings.Contains(w.Body.String(), key) {
			t.Fatalf("missing group field %s: %s", key, w.Body)
		}
	}
	w = apiRequest(t, e, owner, "GET", "/v1/groups/a", "")
	var group Group
	if w.Code != http.StatusOK {
		t.Fatalf("group detail: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &group); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(group, cfg.Groups[0]) {
		t.Fatalf("incorrect group detail: %+v", group)
	}
	w = apiRequest(t, e, owner, "GET", "/v1/groups/a/devices", "")
	var devices []Device
	if w.Code != http.StatusOK {
		t.Fatalf("group devices: %d %s", w.Code, w.Body)
	}
	if err := json.Unmarshal(w.Body.Bytes(), &devices); err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 || devices[0].Serial != "A" || devices[0].State != "disconnected" || devices[1].Serial != "B" || devices[1].Writer != lease.ID {
		t.Fatalf("incorrect device inventory: %+v", devices)
	}
	w = apiRequest(t, e, owner, "GET", "/v1/groups/empty/devices", "")
	if w.Code != http.StatusOK || strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("empty group must return an empty array: %d %s", w.Code, w.Body)
	}
	// Shared device A is also visible to b, while b's device C stays hidden from a.
	w = apiRequest(t, e, "spiffe://test/bob", "GET", "/v1/groups/b/devices", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"serial":"A"`) || strings.Contains(w.Body.String(), `"serial":"B"`) {
		t.Fatalf("shared membership incorrect: %d %s", w.Code, w.Body)
	}
}

func TestAPIGroupAuthorizationAndReadOnlyMethods(t *testing.T) {
	e := engineFixture(t)
	for _, path := range []string{"/v1/groups", "/v1/groups/a", "/v1/groups/a/devices"} {
		t.Run(path, func(t *testing.T) {
			w := apiRequest(t, e, "", "GET", path, "")
			if w.Code != http.StatusForbidden {
				t.Fatalf("missing client identity accepted: %d", w.Code)
			}
			w = apiRequest(t, e, "spiffe://test/alice", "POST", path, `{}`)
			if w.Code != http.StatusMethodNotAllowed {
				t.Fatalf("write method accepted: %d", w.Code)
			}
		})
	}
	for _, suffix := range []string{"", "/devices"} {
		hidden := apiRequest(t, e, "spiffe://test/bob", "GET", "/v1/groups/a"+suffix, "")
		missing := apiRequest(t, e, "spiffe://test/bob", "GET", "/v1/groups/missing"+suffix, "")
		if hidden.Code != http.StatusNotFound || missing.Code != http.StatusNotFound || hidden.Body.String() != missing.Body.String() {
			t.Fatalf("hidden group disclosed: %d %s; missing: %d %s", hidden.Code, hidden.Body, missing.Code, missing.Body)
		}
	}
}

func TestAPIGroupResourcesFollowReload(t *testing.T) {
	e := engineFixture(t)
	owner := "spiffe://test/alice"
	cfg := testConfig()
	cfg.Groups[0].Devices = []string{"B"}
	e.Reload(cfg)
	w := apiRequest(t, e, owner, "GET", "/v1/groups/a/devices", "")
	if w.Code != http.StatusOK || strings.Contains(w.Body.String(), `"serial":"A"`) || !strings.Contains(w.Body.String(), `"serial":"B"`) {
		t.Fatalf("stale membership after reload: %d %s", w.Code, w.Body)
	}
	cfg.Identities[owner] = []string{"b"}
	e.Reload(cfg)
	for _, path := range []string{"/v1/groups/a", "/v1/groups/a/devices"} {
		w = apiRequest(t, e, owner, "GET", path, "")
		if w.Code != http.StatusNotFound {
			t.Fatalf("revoked group still accessible: %d %s", w.Code, w.Body)
		}
	}
	w = apiRequest(t, e, owner, "GET", "/v1/groups", "")
	var groups []Group
	if err := json.Unmarshal(w.Body.Bytes(), &groups); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusOK || len(groups) != 1 || groups[0].Name != "b" {
		t.Fatalf("stale group list after reload: %d %s", w.Code, w.Body)
	}
}
