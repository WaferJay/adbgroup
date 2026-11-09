package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Control struct {
	Version  int    `json:"version"`
	ID       string `json:"id"`
	Op       string `json:"op"`
	Rule     string `json:"rule,omitempty"`
	Previous string `json:"previous,omitempty"`
	Stream   string `json:"stream,omitempty"`
	Port     int    `json:"port,omitempty"`
	Error    string `json:"error,omitempty"`
}
type Peer interface {
	Context() context.Context
	Request(context.Context, Control) (Control, error)
	Reverse(context.Context, string, int) (net.Conn, error)
}
type rule struct {
	id            string
	scope         Scope
	device        Device
	local, remote string
	listener      net.Listener
	reverse       bool
	retiring      atomic.Bool
	cleanup       atomic.Bool
	retryAt       atomic.Int64
}
type ruleOperation struct {
	mu   sync.Mutex
	refs int
}
type ForwardManager struct {
	mu     sync.Mutex
	g      *Gateway
	rules  map[string]*rule
	peers  map[string]Peer
	ops    map[string]*ruleOperation
	Listen func(string, string) (net.Listener, error)
}

func NewForwardManager(g *Gateway) *ForwardManager {
	return &ForwardManager{g: g, rules: map[string]*rule{}, peers: map[string]Peer{}, ops: map[string]*ruleOperation{}, Listen: net.Listen}
}
func tcpPort(s string, zero bool) (int, error) {
	if !strings.HasPrefix(s, "tcp:") {
		return 0, errors.New("only TCP listener/host target supported")
	}
	n, err := strconv.Atoi(strings.TrimPrefix(s, "tcp:"))
	if err != nil || n < 0 || n > 65535 || !zero && n == 0 {
		return 0, errors.New("invalid TCP port")
	}
	return n, nil
}
func splitForward(s string) (local, remote string, no bool, err error) {
	if strings.HasPrefix(s, "norebind:") {
		no = true
		s = strings.TrimPrefix(s, "norebind:")
	}
	p := strings.SplitN(s, ";", 2)
	if len(p) != 2 {
		err = errors.New("invalid forward")
		return
	}
	return p[0], p[1], no, nil
}
func (f *ForwardManager) key(s Scope, local string) string { return "f/" + s.Tunnel + "/" + local }
func (f *ForwardManager) reverseKey(d Device, local string) string {
	return "r/" + d.Serial + "/" + local
}
func (f *ForwardManager) operation(key string) func() {
	f.mu.Lock()
	op := f.ops[key]
	if op == nil {
		op = &ruleOperation{}
		f.ops[key] = op
	}
	op.refs++
	f.mu.Unlock()
	op.mu.Lock()
	return func() {
		op.mu.Unlock()
		f.mu.Lock()
		op.refs--
		if op.refs == 0 {
			delete(f.ops, key)
		}
		f.mu.Unlock()
	}
}
func (f *ForwardManager) Attach(id string, p Peer) { f.mu.Lock(); f.peers[id] = p; f.mu.Unlock() }
func (f *ForwardManager) peer(s Scope) Peer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.peers[s.Tunnel]
}
func (f *ForwardManager) snapshot() []*rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]*rule, 0, len(f.rules))
	for _, r := range f.rules {
		out = append(out, r)
	}
	return out
}
func (f *ForwardManager) find(key string) *rule {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rules[key]
}
func (f *ForwardManager) List(c io.Writer, s Scope) {
	var lines []string
	for _, r := range f.snapshot() {
		if !r.reverse && r.scope == s && f.valid(r) {
			lines = append(lines, fmt.Sprintf("%s %s %s\n", r.device.Serial, r.local, r.remote))
		}
	}
	sort.Strings(lines)
	okay(c)
	WriteFrame(c, strings.Join(lines, ""))
}
func (f *ForwardManager) Forward(ctx context.Context, c io.Writer, s Scope, d Device, spec string) {
	unlock := f.operation("f/" + s.Tunnel)
	defer unlock()
	local, remote, no, err := splitForward(spec)
	if err != nil {
		fail(c, err)
		return
	}
	port, err := tcpPort(local, true)
	if err != nil {
		fail(c, err)
		return
	}
	if !(strings.HasPrefix(remote, "tcp:") || strings.HasPrefix(remote, "localabstract:") || strings.HasPrefix(remote, "jdwp:")) || strings.ContainsAny(remote, "\r\n\x00;") {
		fail(c, errors.New("unsupported device target"))
		return
	}
	p := f.peer(s)
	if p == nil {
		fail(c, errors.New("tunnel closed"))
		return
	}
	local = "tcp:" + strconv.Itoa(port)
	key := f.key(s, local)
	old := f.find(key)
	if old != nil && !f.valid(old) {
		// A disconnected device's rule is gone even when its asynchronous
		// cleanup has not run yet. Finish removing its client listener before
		// binding a fresh one, including for --no-rebind and a new device.
		if err := f.removeForward(ctx, key, old); err != nil {
			fail(c, err)
			return
		}
		old = nil
	}
	if old != nil && (no || old.device.Serial != d.Serial) {
		fail(c, errors.New("cannot rebind socket"))
		return
	}
	r := &rule{id: randomID(), scope: s, device: d, remote: remote}
	req := Control{Op: "bind", Rule: r.id, Port: port}
	if old != nil {
		req.Previous = old.id
	}
	reply, err := p.Request(ctx, req)
	if err != nil {
		fail(c, err)
		return
	}
	if ctx.Err() != nil {
		p.Request(context.Background(), Control{Op: "remove", Rule: r.id})
		fail(c, ctx.Err())
		return
	}
	if reply.Port < 1 || reply.Port > 65535 || port != 0 && reply.Port != port {
		p.Request(context.Background(), Control{Op: "remove", Rule: r.id})
		fail(c, errors.New("client returned invalid port"))
		return
	}
	r.local = "tcp:" + strconv.Itoa(reply.Port)
	key = f.key(s, r.local)
	f.mu.Lock()
	live := f.peers[s.Tunnel] != nil
	if live {
		f.rules[key] = r
	}
	f.mu.Unlock()
	if !live {
		p.Request(ctx, Control{Op: "remove", Rule: r.id})
		fail(c, errors.New("tunnel closed"))
		return
	}
	if _, err = p.Request(ctx, Control{Op: "activate", Rule: r.id}); err != nil {
		f.mu.Lock()
		if f.rules[key] == r {
			if old != nil && f.peers[s.Tunnel] != nil {
				f.rules[key] = old
			} else {
				delete(f.rules, key)
			}
		}
		f.mu.Unlock()
		p.Request(context.Background(), Control{Op: "remove", Rule: r.id})
		fail(c, err)
		return
	}
	okay(c)
	okay(c)
	if port == 0 {
		WriteFrame(c, strconv.Itoa(reply.Port))
	}
}
func (f *ForwardManager) Remove(c io.Writer, s Scope, d Device, local string) {
	unlock := f.operation("f/" + s.Tunnel)
	defer unlock()
	n, err := tcpPort(local, false)
	if err != nil {
		fail(c, err)
		return
	}
	key := f.key(s, "tcp:"+strconv.Itoa(n))
	r := f.find(key)
	if r == nil || r.device.Serial != d.Serial || r.scope != s {
		fail(c, errors.New("forward not found"))
		return
	}
	if err := f.removeForward(context.Background(), key, r); err != nil {
		fail(c, err)
		return
	}
	okay(c)
	okay(c)
}
func (f *ForwardManager) RemoveAll(ctx context.Context, c io.Writer, s Scope, selected *Device) {
	unlock := f.operation("f/" + s.Tunnel)
	defer unlock()
	for _, r := range f.snapshot() {
		if r.reverse || r.scope != s || selected != nil && selected.Serial != r.device.Serial {
			continue
		}
		var release func()
		if selected == nil {
			closer, ok := c.(io.Closer)
			if !ok {
				fail(c, errors.New("connection required"))
				return
			}
			if p, ok := closer.(*pumpConn); ok {
				closer = p.accessCloser()
			}
			wait, cancel := context.WithTimeout(ctx, f.g.Engine.Config().ReadWait())
			var err error
			release, err = f.g.Engine.Acquire(wait, s, r.device, closer)
			cancel()
			if err != nil {
				fail(c, err)
				return
			}
		}
		err := f.removeForward(ctx, f.key(s, r.local), r)
		if release != nil {
			release()
		}
		if err != nil {
			fail(c, err)
			return
		}
	}
	okay(c)
	okay(c)
}
func (f *ForwardManager) removeRule(ctx context.Context, key string, r *rule) error {
	if r.reverse {
		unlock := f.operation("r/" + r.device.Serial)
		defer unlock()
		return f.removeReverse(ctx, key, r)
	}
	unlock := f.operation("f/" + r.scope.Tunnel)
	defer unlock()
	return f.removeForward(ctx, key, r)
}

// The caller holds the tunnel operation lock through the client's reply, so
// background cleanup cannot close a listener being rebound by Forward.
func (f *ForwardManager) removeForward(ctx context.Context, key string, r *rule) error {
	f.mu.Lock()
	if f.rules[key] != r {
		f.mu.Unlock()
		return nil
	}
	delete(f.rules, key)
	p := f.peers[r.scope.Tunnel]
	f.mu.Unlock()
	if p != nil {
		_, err := p.Request(ctx, Control{Op: "remove", Rule: r.id})
		return err
	}
	return nil
}
func (f *ForwardManager) removeReverse(ctx context.Context, key string, r *rule) error {
	f.mu.Lock()
	if f.rules[key] != r {
		f.mu.Unlock()
		return nil
	}
	f.mu.Unlock()
	r.retiring.Store(true)
	// Each rule has a random Unix socket name, never a recycled TCP port. Even
	// if adbd applies a command but loses its reply, a stale mapping cannot reach
	// another rule, process or group after this listener is closed.
	r.listener.Close()
	if f.g.Engine.DeviceMatches(r.device) {
		if _, err := f.reverseCommand(ctx, r.device, "killforward:"+r.local); err != nil {
			// A lost acknowledgement may mean the listener is already gone. Query
			// adbd before retrying, without treating an arbitrary FAIL as success.
			present, queryErr := f.reversePresent(ctx, r)
			if queryErr != nil || present {
				r.retryAt.Store(time.Now().Add(time.Second).UnixNano())
				return fmt.Errorf("remove reverse %s: %w", r.local, err)
			}
		}
	}
	f.mu.Lock()
	delete(f.rules, key)
	f.mu.Unlock()
	if p := f.peer(r.scope); p != nil {
		p.Request(ctx, Control{Op: "forget", Rule: r.id})
	}
	return nil
}

func (f *ForwardManager) reversePresent(ctx context.Context, r *rule) (bool, error) {
	c, err := f.g.Backend.Device(ctx, r.device, "reverse:list-forward")
	if err != nil {
		return false, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	list, err := ReadFrame(c)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(strings.TrimSpace(list), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 3 {
			return false, errors.New("invalid reverse list from backend")
		}
		if fields[1] == r.local && fields[2] == "localabstract:adbgroup-"+r.id {
			return true, nil
		}
	}
	return false, nil
}
func (f *ForwardManager) valid(r *rule) bool {
	if r.retiring.Load() || f.peer(r.scope) == nil || !f.g.Engine.AuthorizedScope(r.scope) {
		return false
	}
	d, err := f.g.Engine.Select(r.scope, r.device.Serial, "", "")
	return err == nil && d.Generation == r.device.Generation && d.State == "device"
}
func (f *ForwardManager) OpenForward(parent context.Context, s Scope, id string, raw net.Conn) error {
	ctx, c := watch(parent, raw)
	defer c.finish()
	var r *rule
	for _, v := range f.snapshot() {
		if !v.reverse && v.id == id && v.scope == s {
			r = v
			break
		}
	}
	if r == nil || !f.valid(r) {
		return errors.New("forward not found")
	}
	wait, cancel := context.WithTimeout(ctx, f.g.Engine.Config().ReadWait())
	release, err := f.g.Engine.Acquire(wait, s, r.device, c.accessCloser())
	cancel()
	if err != nil {
		return err
	}
	defer release()
	slog.Info("forward access", "identity", s.Identity, "group", s.Group, "device", r.device.Serial, "tunnel", s.Tunnel, "lease", s.Lease)
	up, err := f.g.Backend.Device(ctx, r.device, r.remote)
	if err != nil {
		return err
	}
	defer up.Close()
	bridge(c, up)
	return nil
}
func (f *ForwardManager) reverseCommand(ctx context.Context, d Device, cmd string) (string, error) {
	c, err := f.g.Backend.Device(ctx, d, "reverse:"+cmd)
	if err != nil {
		return "", err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if err = readStatus(c); err != nil {
		return "", err
	}
	if strings.HasPrefix(cmd, "forward:") && strings.Contains(cmd, "tcp:0;") {
		return ReadFrame(c)
	}
	return "", nil
}
func (f *ForwardManager) Reverse(ctx context.Context, c io.Writer, s Scope, d Device, cmd string) {
	unlock := f.operation("r/" + d.Serial)
	defer unlock()
	if cmd == "list-forward" {
		var lines []string
		for _, r := range f.snapshot() {
			if r.reverse && !r.retiring.Load() && r.device.Serial == d.Serial {
				lines = append(lines, fmt.Sprintf("host %s %s\n", r.local, r.remote))
			}
		}
		sort.Strings(lines)
		okay(c)
		WriteFrame(c, strings.Join(lines, ""))
		return
	}
	if cmd == "killforward-all" || strings.HasPrefix(cmd, "killforward:") {
		local := strings.TrimPrefix(cmd, "killforward:")
		if cmd != "killforward-all" {
			n, err := tcpPort(local, false)
			if err != nil {
				fail(c, err)
				return
			}
			local = "tcp:" + strconv.Itoa(n)
		}
		found := false
		var cleanupErr error
		for _, r := range f.snapshot() {
			if r.reverse && r.device.Serial == d.Serial && (cmd == "killforward-all" || r.local == local) {
				found = true
				if err := f.removeReverse(ctx, f.reverseKey(d, r.local), r); err != nil && cleanupErr == nil {
					cleanupErr = err
				}
			}
		}
		if cleanupErr != nil {
			fail(c, cleanupErr)
			return
		}
		if !found && cmd != "killforward-all" {
			fail(c, errors.New("reverse not found"))
			return
		}
		okay(c)
		okay(c)
		return
	}
	if !strings.HasPrefix(cmd, "forward:") {
		fail(c, errors.New("reverse command not supported"))
		return
	}
	local, remote, no, err := splitForward(strings.TrimPrefix(cmd, "forward:"))
	if err != nil {
		fail(c, err)
		return
	}
	port, err := tcpPort(local, true)
	if err != nil {
		fail(c, err)
		return
	}
	target, err := tcpPort(remote, false)
	if err != nil {
		fail(c, err)
		return
	}
	local = "tcp:" + strconv.Itoa(port)
	remote = "tcp:" + strconv.Itoa(target)
	key := f.reverseKey(d, local)
	old := f.find(key)
	if old != nil && no {
		fail(c, errors.New("cannot rebind existing socket"))
		return
	}
	p := f.peer(s)
	if p == nil {
		fail(c, errors.New("tunnel closed"))
		return
	}
	r := &rule{id: randomID(), scope: s, device: d, local: local, remote: remote, reverse: true}
	if _, err = p.Request(ctx, Control{Op: "remember", Rule: r.id, Port: target}); err != nil {
		fail(c, err)
		return
	}
	// Linux abstract Unix sockets avoid filesystem cleanup and, unlike ephemeral
	// TCP ports, cannot be reused by unrelated rules after a failed ADB operation.
	name := "adbgroup-" + r.id
	l, err := f.Listen("unix", "@"+name)
	if err != nil {
		p.Request(ctx, Control{Op: "forget", Rule: r.id})
		fail(c, err)
		return
	}
	r.listener = l
	prefix := "forward:"
	if no {
		prefix += "norebind:"
	}
	actual, err := f.reverseCommand(ctx, d, prefix+local+";localabstract:"+name)
	if err != nil {
		l.Close()
		p.Request(ctx, Control{Op: "forget", Rule: r.id})
		fail(c, err)
		return
	}
	if port == 0 {
		n, e := strconv.Atoi(actual)
		if e != nil || n < 1 || n > 65535 {
			l.Close()
			p.Request(ctx, Control{Op: "forget", Rule: r.id})
			fail(c, errors.New("invalid allocated reverse port"))
			return
		}
		r.local = "tcp:" + strconv.Itoa(n)
		key = f.reverseKey(d, r.local)
		old = f.find(key)
	}
	f.mu.Lock()
	live := f.peers[s.Tunnel] != nil
	f.rules[key] = r
	f.mu.Unlock()
	if old != nil {
		old.listener.Close()
		if previous := f.peer(old.scope); previous != nil {
			previous.Request(ctx, Control{Op: "forget", Rule: old.id})
		}
	}
	if !live {
		if err := f.removeReverse(context.Background(), key, r); err != nil {
			slog.Warn("reverse cleanup pending", "device", d.Serial, "error", err)
		}
		fail(c, errors.New("tunnel closed"))
		return
	}
	go f.serveReverse(r)
	okay(c)
	okay(c)
	if port == 0 {
		WriteFrame(c, strings.TrimPrefix(r.local, "tcp:"))
	}
}
func (f *ForwardManager) serveReverse(r *rule) {
	for {
		c, err := r.listener.Accept()
		if err != nil {
			return
		}
		go f.reverseData(r, c)
	}
}
func (f *ForwardManager) reverseData(r *rule, raw net.Conn) {
	p := f.peer(r.scope)
	if p == nil {
		raw.Close()
		return
	}
	ctx, c := watch(p.Context(), raw)
	defer c.finish()
	if !f.valid(r) {
		return
	}
	wait, cancel := context.WithTimeout(ctx, f.g.Engine.Config().ReadWait())
	release, err := f.g.Engine.Acquire(wait, r.scope, r.device, c.accessCloser())
	cancel()
	if err != nil {
		return
	}
	defer release()
	if r.retiring.Load() || f.find(f.reverseKey(r.device, r.local)) != r {
		return
	}
	slog.Info("reverse access", "identity", r.scope.Identity, "group", r.scope.Group, "device", r.device.Serial, "tunnel", r.scope.Tunnel, "lease", r.scope.Lease)
	port, _ := tcpPort(r.remote, false)
	up, err := p.Reverse(ctx, r.id, port)
	if err != nil {
		return
	}
	defer up.Close()
	bridge(c, up)
}
func (f *ForwardManager) Drop(id string) {
	f.mu.Lock()
	delete(f.peers, id)
	f.mu.Unlock()
	for _, r := range f.snapshot() {
		if r.scope.Tunnel == id {
			key := f.key(r.scope, r.local)
			if r.reverse {
				key = f.reverseKey(r.device, r.local)
			}
			f.removeRule(context.Background(), key, r)
		}
	}
}
func (f *ForwardManager) Sweep(ctx context.Context) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, r := range f.snapshot() {
				if !f.valid(r) && time.Now().UnixNano() >= r.retryAt.Load() && r.cleanup.CompareAndSwap(false, true) {
					key := f.key(r.scope, r.local)
					if r.reverse {
						key = f.reverseKey(r.device, r.local)
					}
					go func() {
						defer r.cleanup.Store(false)
						if err := f.removeRule(ctx, key, r); err != nil {
							slog.Warn("forward cleanup pending", "device", r.device.Serial, "error", err)
						}
					}()
				}
			}
		}
	}
}
func (f *ForwardManager) Close() {
	f.mu.Lock()
	f.peers = map[string]Peer{}
	f.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range f.snapshot() {
		wg.Add(1)
		go func(r *rule) {
			defer wg.Done()
			key := f.key(r.scope, r.local)
			if r.reverse {
				key = f.reverseKey(r.device, r.local)
			}
			f.removeRule(context.Background(), key, r)
		}(r)
	}
	wg.Wait()
	f.mu.Lock()
	f.rules = map[string]*rule{}
	f.mu.Unlock()
}
