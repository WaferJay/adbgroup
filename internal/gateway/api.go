package gateway

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

type API struct {
	Engine  *Engine
	Tunnels http.Handler
}

func (a API) Handler() http.Handler {
	mux := http.NewServeMux()
	if a.Tunnels != nil {
		mux.Handle("/v1/groups/{group}/tunnels", a.Tunnels)
		mux.Handle("/v1/groups/{group}/tunnels/{rest...}", a.Tunnels)
	}
	mux.HandleFunc("GET /v1/groups", a.groups)
	mux.HandleFunc("GET /v1/groups/{group}", a.group)
	mux.HandleFunc("GET /v1/groups/{group}/devices", a.groupDevices)
	devicePath := "/v1/groups/{group}/devices/{device}"
	leasePath := devicePath + "/write-leases"
	mux.HandleFunc("GET "+devicePath, a.device)
	mux.HandleFunc("POST "+leasePath, a.createDeviceLease)
	mux.HandleFunc("GET "+leasePath+"/{lease}", a.get)
	mux.HandleFunc("PATCH "+leasePath+"/{lease}", a.patchLease)
	mux.HandleFunc("DELETE "+leasePath+"/{lease}", a.remove)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		identity := APIIdentity(r)
		if identity == "" || len(a.Engine.Config().Identities[identity]) == 0 {
			apiError(w, http.StatusForbidden, "client certificate identity not authorized")
			return
		}
		mux.ServeHTTP(w, r)
	})
}
func APIIdentity(r *http.Request) string {
	if r.TLS == nil || len(r.TLS.VerifiedChains) == 0 || len(r.TLS.PeerCertificates) == 0 {
		return ""
	}
	cert := r.TLS.PeerCertificates[0]
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return ""
	}
	if len(cert.URIs) != 1 {
		return ""
	}
	return cert.URIs[0].String()
}
func apiError(w http.ResponseWriter, status int, msg string) {
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		apiError(w, 400, err.Error())
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		apiError(w, 400, "one JSON value required")
		return false
	}
	return true
}

type leaseOptions struct {
	WaitSeconds int `json:"wait_seconds"`
	TTLSeconds  int `json:"ttl_seconds"`
}

func leaseTTL(n int) (time.Duration, error) {
	if n == 0 {
		n = 300
	}
	if n < 1 || n > 1800 {
		return 0, fmt.Errorf("ttl_seconds must be between 1 and 1800")
	}
	return time.Duration(n) * time.Second, nil
}
func (a API) issueLease(w http.ResponseWriter, r *http.Request, group, serial string, in leaseOptions) {
	if in.WaitSeconds == 0 {
		in.WaitSeconds = 30
	}
	if in.WaitSeconds < 1 || in.WaitSeconds > 3600 {
		apiError(w, 400, "wait_seconds must be between 1 and 3600")
		return
	}
	ttl, err := leaseTTL(in.TTLSeconds)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	l, err := a.Engine.CreateLease(APIIdentity(r), group, serial, time.Duration(in.WaitSeconds)*time.Second, ttl)
	if err != nil {
		apiError(w, 409, err.Error())
		return
	}
	w.Header().Set("Location", "/v1/groups/"+url.PathEscape(l.Group)+"/devices/"+url.PathEscape(l.Serial)+"/write-leases/"+url.PathEscape(l.ID))
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(struct {
		Lease Lease `json:"lease"`
	}{l})
}
func (a API) owned(w http.ResponseWriter, r *http.Request) (Lease, bool) {
	l, ok := a.Engine.Lease(r.PathValue("lease"))
	if !ok || l.Owner != APIIdentity(r) {
		apiError(w, 404, "lease not found")
		return Lease{}, false
	}
	group := r.PathValue("group")
	cfg := a.Engine.Config()
	g, exists := cfg.Group(group)
	if l.Group != group || l.Serial != r.PathValue("device") || !exists || !g.Contains(l.Serial) || !cfg.Allows(APIIdentity(r), group) {
		apiError(w, http.StatusNotFound, "lease not found")
		return Lease{}, false
	}
	return l, true
}
func (a API) get(w http.ResponseWriter, r *http.Request) {
	if l, ok := a.owned(w, r); ok {
		json.NewEncoder(w).Encode(l)
	}
}
func (a API) remove(w http.ResponseWriter, r *http.Request) {
	lease, ok := a.owned(w, r)
	if !ok {
		return
	}
	if err := a.Engine.ReleaseLease(lease.ID, APIIdentity(r)); err != nil {
		apiError(w, 404, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (a API) groups(w http.ResponseWriter, r *http.Request) {
	cfg := a.Engine.Config()
	groups := make([]Group, 0)
	for _, group := range cfg.Groups {
		if cfg.Allows(APIIdentity(r), group.Name) {
			group.Devices = append([]string{}, group.Devices...)
			groups = append(groups, group)
		}
	}
	json.NewEncoder(w).Encode(groups)
}

func (a API) group(w http.ResponseWriter, r *http.Request) {
	group, _, ok := a.Engine.GroupInventory(APIIdentity(r), r.PathValue("group"))
	if !ok {
		apiError(w, http.StatusNotFound, "group not found")
		return
	}
	json.NewEncoder(w).Encode(group)
}

func (a API) groupDevices(w http.ResponseWriter, r *http.Request) {
	_, devices, ok := a.Engine.GroupInventory(APIIdentity(r), r.PathValue("group"))
	if !ok {
		apiError(w, http.StatusNotFound, "group not found")
		return
	}
	json.NewEncoder(w).Encode(devices)
}

func (a API) deviceResource(w http.ResponseWriter, r *http.Request) (Group, Device, bool) {
	group, devices, ok := a.Engine.GroupInventory(APIIdentity(r), r.PathValue("group"))
	if ok {
		for _, device := range devices {
			if device.Serial == r.PathValue("device") {
				return group, device, true
			}
		}
	}
	apiError(w, http.StatusNotFound, "device not found")
	return Group{}, Device{}, false
}

type deviceAccess struct {
	Type    string `json:"type"`
	Tunnels string `json:"tunnels"`
}

func (a API) device(w http.ResponseWriter, r *http.Request) {
	group, device, ok := a.deviceResource(w, r)
	if !ok {
		return
	}
	json.NewEncoder(w).Encode(struct {
		Device
		Group  string       `json:"group"`
		Access deviceAccess `json:"access"`
	}{Device: device, Group: group.Name, Access: deviceAccess{"mtls", "/v1/groups/" + url.PathEscape(group.Name) + "/tunnels"}})
}

func (a API) createDeviceLease(w http.ResponseWriter, r *http.Request) {
	group, device, ok := a.deviceResource(w, r)
	if !ok {
		return
	}
	var in leaseOptions
	if !decodeJSON(w, r, &in) {
		return
	}
	a.issueLease(w, r, group.Name, device.Serial, in)
}

func (a API) patchLease(w http.ResponseWriter, r *http.Request) {
	lease, ok := a.owned(w, r)
	if !ok {
		return
	}
	var in struct {
		ExpiresAt *time.Time `json:"expires_at"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.ExpiresAt == nil {
		apiError(w, http.StatusBadRequest, "expires_at is required")
		return
	}
	l, err := a.Engine.RenewUntil(lease.ID, APIIdentity(r), *in.ExpiresAt)
	if err != nil {
		if err == ErrInvalidExpiry {
			apiError(w, http.StatusBadRequest, err.Error())
		} else {
			apiError(w, http.StatusConflict, err.Error())
		}
		return
	}
	json.NewEncoder(w).Encode(l)
}
