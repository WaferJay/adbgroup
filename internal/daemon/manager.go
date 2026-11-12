// Package daemon manages independent client tunnels for one local user.
package daemon

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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
	config Config
	info   Tunnel
	cancel context.CancelFunc
	ready  chan struct{}
	done   chan struct{}
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
	return newManager(func(ctx context.Context, cfg Config, ready func(string, string)) error {
		c, err := tunnel.NewClient(cfg.Server, cfg.Cert, cfg.Key, cfg.CA)
		if err != nil {
			return err
		}
		if cfg.Listen == "" {
			return c.RunReadyAuto(ctx, cfg.Group, ready)
		}
		return c.RunReady(ctx, cfg.Group, cfg.Lease, cfg.Listen, ready)
	})
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
	rank := map[string]int{"active": 0, "starting": 1, "failed": 2}
	var selected *entry
	for _, e := range m.entries {
		candidate := e.config
		candidate.Listen = ""
		candidate.Group = e.info.Group
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
	e := &entry{config: cfg, info: Tunnel{ID: id, Server: cfg.Server, Group: cfg.Group, Listen: cfg.Listen, State: "starting"}, cancel: cancel, ready: make(chan struct{}), done: make(chan struct{})}
	m.entries[id] = e
	go m.execute(runCtx, e)
	return e
}

func (m *Manager) wait(ctx context.Context, e *entry, owner bool) (Tunnel, error) {
	select {
	case <-ctx.Done():
		m.mu.Lock()
		if owner && e.info.State == "starting" {
			e.cancel()
		}
		m.mu.Unlock()
		return Tunnel{}, ctx.Err()
	case <-e.ready:
	case <-e.done:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.info.State != "active" {
		return e.info, fmt.Errorf("tunnel %s: %s", e.info.ID, e.info.Error)
	}
	return e.info, nil
}

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
	err := m.run(ctx, e.config, func(group, address string) {
		m.mu.Lock()
		defer m.mu.Unlock()
		if e.info.State != "starting" || ctx.Err() != nil {
			return
		}
		timer.Stop()
		e.config.Listen = address
		e.info.Group, e.info.Listen, e.info.State = group, address, "active"
		close(e.ready)
	})
	timer.Stop()
	e.cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.info.Error == "" {
		if err != nil {
			e.info.Error = err.Error()
		} else {
			e.info.Error = "tunnel closed"
		}
	}
	if e.info.State != "stopping" {
		e.info.State = "failed"
	}
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
	m.mu.Unlock()
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
	}
	m.mu.Unlock()
	for _, e := range entries {
		<-e.done
	}
	m.mu.Lock()
	clear(m.entries)
	m.mu.Unlock()
}
