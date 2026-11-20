// Package daemon manages independent client tunnels for one local user.
package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/WaferJay/adbgroup/internal/tunnel"
)

const SetupTimeout = 30 * time.Second

type Config struct {
	Server string `json:"server"`
	Group  string `json:"group"`
	Lease  string `json:"lease,omitempty"`
	Listen string `json:"listen"`
	Cert   string `json:"cert"`
	Key    string `json:"key"`
	CA     string `json:"ca"`
}

func (c *Config) Normalize() error {
	if c.Group == "" {
		return errors.New("group required")
	}
	u, err := url.Parse(c.Server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return errors.New("server must be an HTTPS origin")
	}
	c.Server = strings.TrimSuffix(c.Server, "/")
	c.Listen, err = tunnel.LocalAddress(c.Listen)
	if err != nil {
		return err
	}
	for _, path := range []string{c.Cert, c.Key, c.CA} {
		if !filepath.IsAbs(path) {
			return errors.New("credential paths must be absolute")
		}
	}
	c.Cert, c.Key, c.CA = filepath.Clean(c.Cert), filepath.Clean(c.Key), filepath.Clean(c.CA)
	return nil
}

type Tunnel struct {
	ID     string `json:"id"`
	Server string `json:"server"`
	Group  string `json:"group"`
	Listen string `json:"listen"`
	State  string `json:"state"`
	Error  string `json:"error,omitempty"`
}

type entry struct {
	config      Config
	info        Tunnel
	cancel      context.CancelFunc
	changed     chan struct{}
	done        chan struct{}
	endpoint    *tunnel.Endpoint
	established bool
}

type runner func(context.Context, Config, func(string, string)) error

type Manager struct {
	mu           sync.Mutex
	entries      map[string]*entry
	closed       bool
	run          runner
	setupTimeout time.Duration
}

func NewManager() *Manager {
	return newManager(nil)
}

func newManager(run runner) *Manager {
	return &Manager{entries: make(map[string]*entry), run: run, setupTimeout: SetupTimeout}
}

func (m *Manager) Start(ctx context.Context, cfg Config) (Tunnel, error) {
	if err := cfg.Normalize(); err != nil {
		return Tunnel{}, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Tunnel{}, errors.New("daemon is stopping")
	}
	var e *entry
	for _, existing := range m.entries {
		if existing.config.Listen != cfg.Listen {
			continue
		}
		if existing.config != cfg {
			m.mu.Unlock()
			return Tunnel{}, fmt.Errorf("listener %s already belongs to tunnel %s with different configuration", cfg.Listen, existing.info.ID)
		}
		if existing.info.State == "stopping" {
			m.mu.Unlock()
			return Tunnel{}, errors.New("tunnel is stopping")
		}
		e = existing
		break
	}
	owner := e == nil || e.info.State == "failed"
	if owner {
		e = m.startLocked(cfg, e)
	}
	m.mu.Unlock()
	return m.wait(ctx, e, owner)
}

// Ensure reuses a normal tunnel for this connection and group, or binds a new
// OS-assigned port. Lookup and creation are atomic with respect to Start/Ensure.
func (m *Manager) Ensure(ctx context.Context, cfg Config) (Tunnel, error) {
	if cfg.Group == "" || cfg.Lease != "" {
		return Tunnel{}, errors.New("ensure requires a group and no lease")
	}
	cfg.Listen = "127.0.0.1:1" // Validate connection fields without accepting port 0 in Start.
	if err := cfg.Normalize(); err != nil {
		return Tunnel{}, err
	}
	cfg.Listen = ""
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return Tunnel{}, errors.New("daemon is stopping")
	}
	rank := map[string]int{"active": 0, "starting": 1, "reconnecting": 2, "failed": 3}
	var selected *entry
	for _, e := range m.entries {
		candidate := e.config
		candidate.Listen = ""
		r, ok := rank[e.info.State]
		if candidate != cfg || !ok {
			continue
		}
		if selected == nil || r < rank[selected.info.State] || r == rank[selected.info.State] && e.info.ID < selected.info.ID {
			selected = e
		}
	}
	owner := selected == nil || selected.info.State == "failed"
	if owner {
		if selected != nil {
			cfg.Listen = selected.config.Listen
		}
		selected = m.startLocked(cfg, selected)
	}
	m.mu.Unlock()
	return m.wait(ctx, selected, owner)
}

// Caller holds m.mu.
func (m *Manager) startLocked(cfg Config, previous *entry) *entry {
	id := fmt.Sprintf("%x", randomID())
	if previous != nil {
		id = previous.info.ID
	}
	runCtx, cancel := context.WithCancel(context.Background())
	e := &entry{config: cfg, info: Tunnel{ID: id, Server: cfg.Server, Group: cfg.Group, Listen: cfg.Listen, State: "starting"}, cancel: cancel, changed: make(chan struct{}), done: make(chan struct{})}
	if m.run == nil && cfg.Lease == "" {
		if previous != nil && previous.established {
			e.endpoint = previous.endpoint
			e.established = true
		}
		if e.endpoint == nil {
			e.endpoint = tunnel.NewEndpoint(cfg.Listen, func() (*tunnel.Client, error) {
				return tunnel.NewClient(cfg.Server, cfg.Cert, cfg.Key, cfg.CA)
			})
		}
	}
	m.entries[id] = e
	go m.execute(runCtx, e)
	return e
}

func (m *Manager) wait(ctx context.Context, e *entry, owner bool) (Tunnel, error) {
	for {
		m.mu.Lock()
		info, changed := e.info, e.changed
		m.mu.Unlock()
		if info.State == "active" {
			return info, nil
		}
		if info.State != "starting" && info.State != "reconnecting" {
			return info, fmt.Errorf("tunnel %s: %s", info.ID, info.Error)
		}
		select {
		case <-ctx.Done():
			m.mu.Lock()
			if owner && e.info.State == "starting" {
				e.cancel()
			}
			m.mu.Unlock()
			return Tunnel{}, ctx.Err()
		case <-changed:
		case <-e.done:
		}
	}
}

func (e *entry) signal() { close(e.changed); e.changed = make(chan struct{}) }

func randomID() []byte {
	b := make([]byte, 12)
	// crypto/rand.Read never fails on supported platforms.
	rand.Read(b)
	return b
}

func (m *Manager) execute(ctx context.Context, e *entry) {
	timer := time.AfterFunc(m.setupTimeout, func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		if e.info.State == "starting" {
			e.info.Error = "tunnel setup timed out"
			e.cancel()
		}
	})
	ready := func(group, address string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if e.info.State == "stopping" || ctx.Err() != nil {
			return
		}
		timer.Stop()
		e.config.Listen = address
		e.config.Group = group
		e.info.Group, e.info.Listen, e.info.State = group, address, "active"
		e.info.Error = ""
		e.established = true
		e.signal()
	}
	var err error
	if e.endpoint != nil {
		err = e.endpoint.Run(ctx, e.config.Group, ready, func(state string, err error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if e.info.State != "stopping" {
				e.info.State, e.info.Error = state, err.Error()
				e.signal()
			}
		})
	} else if m.run != nil {
		err = m.run(ctx, e.config, ready)
	} else {
		var c *tunnel.Client
		c, err = tunnel.NewClient(e.config.Server, e.config.Cert, e.config.Key, e.config.CA)
		if err == nil {
			err = c.RunReady(ctx, e.config.Group, e.config.Lease, e.config.Listen, ready)
		}
	}
	timer.Stop()
	e.cancel()
	m.mu.Lock()
	abandoned := !e.established && e.endpoint != nil
	m.mu.Unlock()
	if abandoned {
		e.endpoint.Close()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.info.Error == "" || e.info.State == "reconnecting" {
		if err != nil {
			e.info.Error = err.Error()
		} else {
			e.info.Error = "tunnel closed"
		}
	}
	if e.info.State != "stopping" {
		e.info.State = "failed"
		slog.Warn("managed tunnel failed", "id", e.info.ID, "error", e.info.Error)
	}
	e.signal()
	close(e.done)
}

func (m *Manager) List() []Tunnel {
	m.mu.Lock()
	defer m.mu.Unlock()
	list := make([]Tunnel, 0, len(m.entries))
	for _, e := range m.entries {
		list = append(list, e.info)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

func (m *Manager) Stop(ctx context.Context, id string) error {
	m.mu.Lock()
	e := m.entries[id]
	if e == nil {
		m.mu.Unlock()
		return fmt.Errorf("tunnel %s not found", id)
	}
	e.info.State = "stopping"
	e.cancel()
	e.signal()
	m.mu.Unlock()
	if e.endpoint != nil {
		e.endpoint.Close()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
	}
	m.mu.Lock()
	if m.entries[id] == e {
		delete(m.entries, id)
	}
	m.mu.Unlock()
	return nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	entries := make([]*entry, 0, len(m.entries))
	for _, e := range m.entries {
		e.info.State = "stopping"
		e.cancel()
		entries = append(entries, e)
		e.signal()
	}
	m.mu.Unlock()
	for _, e := range entries {
		if e.endpoint != nil {
			e.endpoint.Close()
		}
		<-e.done
	}
	m.mu.Lock()
	clear(m.entries)
	m.mu.Unlock()
}
