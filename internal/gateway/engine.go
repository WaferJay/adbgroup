package gateway

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"
)

var ErrBusy = errors.New("device busy: lock wait expired or queue full")
var ErrInvalidExpiry = errors.New("expires_at must be in the future and at most 30 minutes away")

type Device struct {
	Serial      string `json:"serial"`
	State       string `json:"state"`
	TransportID string `json:"transport_id"`
	Detail      string `json:"-"`
	Generation  uint64 `json:"generation"`
	Readers     int    `json:"readers"`
	Queued      int    `json:"queued_readers"`
	Writer      string `json:"writer,omitempty"`
	Duplicate   bool   `json:"duplicate"`
}
type Scope struct {
	Group    string
	Lease    string
	Identity string
	Tunnel   string
}
type Lease struct {
	ID         string        `json:"id"`
	Owner      string        `json:"-"`
	Group      string        `json:"group"`
	Serial     string        `json:"serial"`
	State      string        `json:"state"`
	Created    time.Time     `json:"created_at"`
	WaitUntil  time.Time     `json:"wait_until"`
	Expires    time.Time     `json:"expires_at,omitempty"`
	TTL        time.Duration `json:"-"`
	Generation uint64        `json:"generation"`
}
type access struct {
	scope      Scope
	serial     string
	close      io.Closer
	read       bool
	generation uint64
	closing    bool
}
type Engine struct {
	mu       sync.Mutex
	cfg      Config
	devices  map[string]*Device
	leases   map[string]*Lease
	queue    map[string][]string
	accesses map[uint64]access
	next     uint64
	changed  chan struct{}
	closing  map[string]int
}

func NewEngine(c Config) *Engine {
	return &Engine{cfg: c, devices: map[string]*Device{}, leases: map[string]*Lease{}, queue: map[string][]string{}, accesses: map[uint64]access{}, changed: make(chan struct{}), closing: map[string]int{}}
}
func (e *Engine) Config() Config           { e.mu.Lock(); defer e.mu.Unlock(); return e.cfg }
func (e *Engine) signal()                  { close(e.changed); e.changed = make(chan struct{}) }
func (e *Engine) Changed() <-chan struct{} { e.mu.Lock(); defer e.mu.Unlock(); return e.changed }
func (e *Engine) permitted(s Scope, serial string) bool {
	if s.Identity == "" || !e.cfg.Allows(s.Identity, s.Group) {
		return false
	}
	g, ok := e.cfg.Group(s.Group)
	if !ok || !g.Contains(serial) {
		return false
	}
	if s.Lease != "" {
		l := e.leases[s.Lease]
		return l != nil && l.Owner == s.Identity && l.Group == s.Group && l.Serial == serial && l.State == "active" && time.Now().Before(l.Expires) && e.cfg.Allows(l.Owner, l.Group)
	}
	return true
}
func (e *Engine) Snapshot(s Scope) []Device {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []Device
	for serial, d := range e.devices {
		if e.permitted(s, serial) {
			out = append(out, *d)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Serial < out[j].Serial })
	return out
}

// GroupInventory checks authorization and reads membership and device state from
// one configuration snapshot, including across concurrent configuration reloads.
func (e *Engine) GroupInventory(identity, name string) (Group, []Device, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	group, ok := e.cfg.Group(name)
	if !ok || !e.cfg.Allows(identity, name) {
		return Group{}, nil, false
	}
	group.Devices = append([]string{}, group.Devices...)
	devices := make([]Device, 0, len(group.Devices))
	for _, serial := range group.Devices {
		if device := e.devices[serial]; device != nil {
			devices = append(devices, *device)
		} else {
			devices = append(devices, Device{Serial: serial, State: "disconnected"})
		}
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].Serial < devices[j].Serial })
	return group, devices, true
}

func (e *Engine) Select(s Scope, serial, id, kind string) (Device, error) {
	var matches []Device
	for _, d := range e.Snapshot(s) {
		if d.Duplicate {
			continue
		}
		if serial != "" && d.Serial != serial || id != "" && d.TransportID != id {
			continue
		}
		if kind == "local" {
			continue
		}
		matches = append(matches, d)
	}
	if len(matches) == 0 {
		return Device{}, errors.New("device not found or not authorized")
	}
	if len(matches) > 1 {
		return Device{}, errors.New("more than one device/emulator")
	}
	return matches[0], nil
}
func (e *Engine) Update(devices []Device) {
	e.mu.Lock()
	defer e.mu.Unlock()
	counts := map[string]int{}
	for _, d := range devices {
		counts[d.Serial]++
	}
	next := map[string]*Device{}
	for _, d := range devices {
		if d.Serial == "" {
			continue
		}
		d.Duplicate = counts[d.Serial] > 1
		old := e.devices[d.Serial]
		if old != nil {
			d.Generation = old.Generation
			d.Readers = old.Readers
			d.Queued = old.Queued
			d.Writer = old.Writer
		}
		if old == nil || old.TransportID != d.TransportID || old.State != d.State || old.Duplicate != d.Duplicate {
			e.invalidate(d.Serial)
			e.next++
			d.Generation = e.next
			d.Readers = 0
			d.Queued = 0
			d.Writer = ""
		}
		copy := d
		next[d.Serial] = &copy
	}
	for serial := range e.devices {
		if next[serial] == nil {
			e.invalidate(serial)
		}
	}
	e.devices = next
	e.signal()
}
func (e *Engine) invalidate(serial string) {
	for id, a := range e.accesses {
		if a.serial == serial {
			e.closeAccess(id)
		}
	}
	for _, l := range e.leases {
		if l.Serial == serial && (l.State == "active" || l.State == "queued") {
			l.State = "invalidated"
		}
	}
	delete(e.queue, serial)
}
func (e *Engine) Reload(c Config) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.cfg = c
	for id, a := range e.accesses {
		if !e.permitted(a.scope, a.serial) {
			e.closeAccess(id)
		}
	}
	for _, l := range e.leases {
		if (l.State == "active" || l.State == "queued") && (!c.Allows(l.Owner, l.Group) || !e.member(l.Group, l.Serial)) {
			e.endLease(l, "revoked")
		}
	}
	e.advance()
	e.signal()
}
func (e *Engine) member(group, serial string) bool {
	g, ok := e.cfg.Group(group)
	return ok && g.Contains(serial)
}
func (e *Engine) Acquire(ctx context.Context, s Scope, d Device, closer io.Closer) (func(), error) {
	e.mu.Lock()
	queued := false
	defer func() {
		if queued {
			if live := e.devices[d.Serial]; live != nil && live.Generation == d.Generation {
				live.Queued--
			}
		}
		e.mu.Unlock()
	}()
	for {
		live := e.devices[d.Serial]
		if !e.permitted(s, d.Serial) || live == nil || live.Generation != d.Generation || live.State != "device" || live.Duplicate {
			return nil, errors.New("device offline, changed or not authorized")
		}
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrBusy, err)
		}
		if e.closing[d.Serial] == 0 && (s.Lease != "" || live.Writer == "" && len(e.queue[d.Serial]) == 0) {
			e.next++
			id := e.next
			read := s.Lease == ""
			if read {
				live.Readers++
			}
			e.accesses[id] = access{scope: s, serial: d.Serial, close: closer, read: read, generation: d.Generation}
			e.signal()
			var once sync.Once
			return func() {
				once.Do(func() {
					e.mu.Lock()
					defer e.mu.Unlock()
					if a, ok := e.accesses[id]; ok {
						if a.closing {
							return
						}
						delete(e.accesses, id)
						if a.read && e.devices[d.Serial] != nil && e.devices[d.Serial].Generation == a.generation {
							e.devices[d.Serial].Readers--
						}
						e.advance()
						e.signal()
					}
				})
			}, nil
		}
		if !queued {
			if live.Queued >= e.cfg.ReadQueueLimit {
				return nil, ErrBusy
			}
			live.Queued++
			queued = true
		}
		ch := e.changed
		e.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-ch:
		}
		e.mu.Lock()
	}
}
func randomID() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func (e *Engine) CreateLease(owner, group, serial string, wait, ttl time.Duration) (Lease, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.cfg.Allows(owner, group) || !e.member(group, serial) {
		return Lease{}, errors.New("not authorized")
	}
	d := e.devices[serial]
	if d == nil || d.State != "device" || d.Duplicate {
		return Lease{}, errors.New("device not available")
	}
	if len(e.queue[serial]) >= 128 {
		return Lease{}, ErrBusy
	}
	now := time.Now()
	l := &Lease{ID: randomID(), Owner: owner, Group: group, Serial: serial, State: "queued", Created: now, WaitUntil: now.Add(wait), TTL: ttl, Generation: d.Generation}
	e.leases[l.ID] = l
	e.queue[serial] = append(e.queue[serial], l.ID)
	e.advance()
	e.signal()
	return *l, nil
}
func (e *Engine) Lease(id string) (Lease, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l, ok := e.leases[id]
	if !ok {
		return Lease{}, false
	}
	return *l, true
}
func (e *Engine) AuthorizedScope(s Scope) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if s.Identity == "" || !e.cfg.Allows(s.Identity, s.Group) {
		return false
	}
	if s.Lease == "" {
		return true
	}
	l := e.leases[s.Lease]
	return l != nil && e.permitted(s, l.Serial)
}
func (e *Engine) RenewUntil(id, owner string, expires time.Time) (Lease, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.leases[id]
	now := time.Now()
	if l == nil || l.Owner != owner || l.State != "active" || !now.Before(l.Expires) {
		return Lease{}, errors.New("active lease not found")
	}
	if !expires.After(now) || expires.After(now.Add(30*time.Minute)) {
		return Lease{}, ErrInvalidExpiry
	}
	l.Expires = expires
	e.signal()
	return *l, nil
}

func (e *Engine) ReleaseLease(id, owner string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.leases[id]
	if l == nil || l.Owner != owner {
		return errors.New("lease not found")
	}
	if l.State == "active" || l.State == "queued" {
		e.endLease(l, "released")
		e.advance()
		e.signal()
	}
	return nil
}
func (e *Engine) endLease(l *Lease, state string) {
	l.State = state
	if d := e.devices[l.Serial]; d != nil && d.Writer == l.ID {
		d.Writer = ""
	}
	for id, a := range e.accesses {
		if a.scope.Lease == l.ID {
			e.closeAccess(id)
		}
	}
}
func (e *Engine) advance() {
	now := time.Now()
	for _, l := range e.leases {
		if l.State == "active" && !now.Before(l.Expires) {
			e.endLease(l, "expired")
		}
		if l.State == "queued" && !now.Before(l.WaitUntil) {
			l.State = "timeout"
		}
	}
	for serial, q := range e.queue {
		var alive []string
		for _, id := range q {
			if e.leases[id].State == "queued" {
				alive = append(alive, id)
			}
		}
		e.queue[serial] = alive
		d := e.devices[serial]
		if d == nil || e.closing[serial] != 0 || d.Readers != 0 || d.Writer != "" || len(alive) == 0 {
			continue
		}
		l := e.leases[alive[0]]
		l.State = "active"
		l.Expires = now.Add(l.TTL)
		d.Writer = l.ID
		e.queue[serial] = alive[1:]
	}
	// Retain terminal states for one hour, then bound in-memory history.
	for id, l := range e.leases {
		if l.State != "active" && l.State != "queued" && now.Sub(l.Created) > time.Hour {
			delete(e.leases, id)
		}
	}
}

// Detach network shutdown from the engine mutex. The per-device barrier keeps
// successors waiting until the old transport has actually been closed.
func (e *Engine) closeAccess(id uint64) {
	a, ok := e.accesses[id]
	if !ok || a.closing {
		return
	}
	a.closing = true
	e.accesses[id] = a
	e.closing[a.serial]++
	go func() {
		a.close.Close()
		e.mu.Lock()
		defer e.mu.Unlock()
		delete(e.accesses, id)
		e.closing[a.serial]--
		if d := e.devices[a.serial]; a.read && d != nil && d.Generation == a.generation {
			d.Readers--
		}
		e.advance()
		e.signal()
	}()
}
func (e *Engine) Run(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			e.Update(nil)
			return
		case <-t.C:
			e.mu.Lock()
			e.advance()
			e.signal()
			e.mu.Unlock()
		}
	}
}

func (e *Engine) DeviceMatches(d Device) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	live := e.devices[d.Serial]
	return live != nil && live.Generation == d.Generation && live.TransportID == d.TransportID && live.State == "device"
}
