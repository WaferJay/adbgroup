// Package tunnel exposes only authenticated, identity-bound ADB streams.
package tunnel

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

const setupTimeout = 10 * time.Second
const maxStreams = 128

func id() string {
	var b [32]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}

type Resource struct {
	ID    string `json:"id"`
	Path  string `json:"path"`
	State string `json:"state,omitempty"`
}
type stream struct {
	Resource
	kind, rule string
	claimed    bool
	conn       net.Conn
	ready      chan net.Conn
	done       chan struct{}
	once       sync.Once
}
type session struct {
	Resource
	scope   gateway.Scope
	expires time.Time
	ctx     context.Context
	cancel  context.CancelFunc
	server  *Server
	mu      sync.Mutex
	control *websocket.Conn
	streams map[string]*stream
	pending map[string]chan gateway.Control
	closed  bool
	cleaned chan struct{}
}
type Server struct {
	Gateway  *gateway.Gateway
	mu       sync.Mutex
	sessions map[string]*session
	ctx      context.Context
}

func NewServer(ctx context.Context, g *gateway.Gateway) *Server {
	return &Server{Gateway: g, ctx: ctx, sessions: map[string]*session{}}
}
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	base := "/v1/groups/{group}/tunnels"
	m.HandleFunc("POST "+base, s.create)
	m.HandleFunc("GET "+base+"/{tunnel}", s.get)
	m.HandleFunc("DELETE "+base+"/{tunnel}", s.remove)
	m.HandleFunc("GET "+base+"/{tunnel}/control", s.control)
	m.HandleFunc("POST "+base+"/{tunnel}/streams", s.createStream)
	m.HandleFunc("GET "+base+"/{tunnel}/streams/{stream}", s.data)
	return m
}
func reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if v != nil {
		json.NewEncoder(w).Encode(v)
	}
}
func bad(w http.ResponseWriter, status int, e string) {
	reply(w, status, map[string]string{"error": e})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		bad(w, 400, e.Error())
		return false
	}
	var extra any
	if e := d.Decode(&extra); e != io.EOF {
		bad(w, 400, "one JSON value required")
		return false
	}
	return true
}
func (s *Server) create(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Lease string `json:"lease_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	scope := gateway.Scope{Group: r.PathValue("group"), Lease: in.Lease, Identity: gateway.APIIdentity(r), Tunnel: id()}
	if !s.Gateway.Engine.AuthorizedScope(scope) {
		bad(w, 404, "group or active lease not found")
		return
	}
	ctx, cancel := context.WithCancel(s.ctx)
	t := &session{Resource: Resource{ID: scope.Tunnel, Path: "/v1/groups/" + url.PathEscape(scope.Group) + "/tunnels/" + scope.Tunnel, State: "pending"}, scope: scope, expires: r.TLS.PeerCertificates[0].NotAfter, ctx: ctx, cancel: cancel, server: s, streams: map[string]*stream{}, pending: map[string]chan gateway.Control{}, cleaned: make(chan struct{})}
	s.mu.Lock()
	count := 0
	for _, other := range s.sessions {
		other.mu.Lock()
		active := !other.closed
		other.mu.Unlock()
		if other.scope.Identity == scope.Identity && active {
			count++
		}
	}
	if count >= 32 {
		s.mu.Unlock()
		cancel()
		bad(w, 429, "too many tunnels")
		return
	}
	s.sessions[t.ID] = t
	s.mu.Unlock()
	out := t.Resource
	go t.monitor()
	w.Header().Set("Location", t.Path)
	reply(w, 201, out)
}
func (s *Server) owned(w http.ResponseWriter, r *http.Request) *session {
	s.mu.Lock()
	t := s.sessions[r.PathValue("tunnel")]
	s.mu.Unlock()
	if t == nil || t.scope.Identity != gateway.APIIdentity(r) || t.scope.Group != r.PathValue("group") || !s.Gateway.Engine.Config().Allows(t.scope.Identity, t.scope.Group) {
		bad(w, 404, "tunnel not found")
		return nil
	}
	t.mu.Lock()
	if until := r.TLS.PeerCertificates[0].NotAfter; until.Before(t.expires) {
		t.expires = until
	}
	t.mu.Unlock()
	if !s.Gateway.Engine.AuthorizedScope(t.scope) {
		t.close()
		if r.Method != "DELETE" && r.URL.EscapedPath() != t.Path {
			bad(w, 404, "tunnel not found")
			return nil
		}
	}
	return t
}
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	if t := s.owned(w, r); t != nil {
		t.mu.Lock()
		out := t.Resource
		t.mu.Unlock()
		reply(w, 200, out)
	}
}
func (s *Server) remove(w http.ResponseWriter, r *http.Request) {
	// Tombstones retain ownership for one hour for idempotent DELETE.
	if t := s.owned(w, r); t != nil {
		t.close()
		select {
		case <-t.cleaned:
		case <-r.Context().Done():
			return
		}
		reply(w, 204, nil)
	}
}
func (t *session) monitor() {
	timer := time.NewTimer(setupTimeout)
	defer timer.Stop()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-t.ctx.Done():
			t.close()
			return
		case <-timer.C:
			t.mu.Lock()
			pending := t.control == nil
			t.mu.Unlock()
			if pending {
				t.close()
				return
			}
		case <-tick.C:
			t.mu.Lock()
			expired := !time.Now().Before(t.expires)
			t.mu.Unlock()
			if expired || !t.server.Gateway.Engine.AuthorizedScope(t.scope) {
				t.close()
				return
			}
		}
	}
}
func (t *session) close() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	t.State = "closed"
	t.cancel()
	control := t.control
	var streams []*stream
	for _, v := range t.streams {
		streams = append(streams, v)
	}
	t.streams = map[string]*stream{}
	t.mu.Unlock()
	if control != nil {
		control.CloseNow()
	}
	for _, v := range streams {
		t.closeStream(v)
	}
	go func() { t.server.Gateway.Forwards.Drop(t.ID); close(t.cleaned) }()
	slog.Info("tunnel closed", "identity", t.scope.Identity, "group", t.scope.Group, "tunnel", t.ID, "lease", t.scope.Lease)
	// Bound ownership tombstones without keeping terminal sessions indefinitely.
	time.AfterFunc(time.Hour, func() { t.server.mu.Lock(); delete(t.server.sessions, t.ID); t.server.mu.Unlock() })
}
func (s *Server) Close() {
	s.mu.Lock()
	all := make([]*session, 0, len(s.sessions))
	for _, t := range s.sessions {
		all = append(all, t)
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, t := range all {
		wg.Add(1)
		go func(t *session) { defer wg.Done(); t.close(); <-t.cleaned }(t)
	}
	wg.Wait()
}
func accept(w http.ResponseWriter, r *http.Request) (*websocket.Conn, error) {
	w.Header().Del("Content-Type")
	// The CLI sends no Origin. Browser cross-origin requests are rejected by default.
	c, e := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if e == nil {
		c.SetReadLimit(64 << 10)
	}
	return c, e
}
func (s *Server) control(w http.ResponseWriter, r *http.Request) {
	t := s.owned(w, r)
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.closed || t.control != nil || t.State != "pending" {
		t.mu.Unlock()
		bad(w, 409, "control already used or closed")
		return
	}
	t.State = "connecting"
	t.mu.Unlock()
	c, err := accept(w, r)
	if err != nil {
		t.close()
		return
	}
	defer t.close()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		c.CloseNow()
		return
	}
	t.control = c
	t.State = "active"
	s.Gateway.Forwards.Attach(t.ID, t)
	t.mu.Unlock()
	if err = t.write(gateway.Control{Version: 1, Op: "ready"}); err != nil {
		return
	}
	go func() {
		tick := time.NewTicker(15 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-tick.C:
				ctx, cancel := context.WithTimeout(t.ctx, setupTimeout)
				err := c.Ping(ctx)
				cancel()
				if err != nil {
					t.close()
					return
				}
			}
		}
	}()
	slog.Info("tunnel active", "identity", t.scope.Identity, "group", t.scope.Group, "tunnel", t.ID, "lease", t.scope.Lease)
	for {
		var m gateway.Control
		if err = wsjson.Read(t.ctx, c, &m); err != nil {
			return
		}
		if m.Version != 1 || m.Op != "reply" {
			return
		}
		t.mu.Lock()
		ch := t.pending[m.ID]
		t.mu.Unlock()
		if ch == nil {
			continue
		}
		select {
		case ch <- m:
		default:
			return
		}
	}
}
func (t *session) Context() context.Context { return t.ctx }
func (t *session) write(m gateway.Control) error {
	t.mu.Lock()
	c := t.control
	closed := t.closed
	t.mu.Unlock()
	if c == nil || closed {
		return errors.New("tunnel closed")
	}
	ctx, cancel := context.WithTimeout(t.ctx, setupTimeout)
	defer cancel()
	return wsjson.Write(ctx, c, m)
}
func (t *session) Request(ctx context.Context, m gateway.Control) (gateway.Control, error) {
	m.Version = 1
	m.ID = id()
	ch := make(chan gateway.Control, 1)
	t.mu.Lock()
	if t.closed || len(t.pending) >= 64 {
		t.mu.Unlock()
		return gateway.Control{}, errors.New("tunnel busy or closed")
	}
	t.pending[m.ID] = ch
	t.mu.Unlock()
	defer func() { t.mu.Lock(); delete(t.pending, m.ID); t.mu.Unlock() }()
	if err := t.write(m); err != nil {
		return gateway.Control{}, err
	}
	timer := time.NewTimer(setupTimeout)
	defer timer.Stop()
	// A reverse stream can be removed to reject late attachments after caller
	// cancellation. Listener mutations must await their reply to avoid orphans.
	var callerDone <-chan struct{}
	if m.Op == "reverse" {
		callerDone = ctx.Done()
	}
	select {
	case <-callerDone:
		return gateway.Control{}, ctx.Err()
	case <-t.ctx.Done():
		return gateway.Control{}, errors.New("tunnel closed")
	case <-timer.C:
		t.close()
		return gateway.Control{}, errors.New("client control timeout")
	case result := <-ch:
		if result.Error != "" {
			return result, errors.New(result.Error)
		}
		return result, nil
	}
}
func (t *session) newStream(kind, rule string) (*stream, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || t.State != "active" || len(t.streams) >= maxStreams {
		return nil, errors.New("tunnel inactive or stream limit reached")
	}
	v := &stream{Resource: Resource{ID: id()}, kind: kind, rule: rule, ready: make(chan net.Conn, 1), done: make(chan struct{})}
	v.Path = t.Path + "/streams/" + v.ID
	t.streams[v.ID] = v
	time.AfterFunc(setupTimeout, func() {
		t.mu.Lock()
		if t.streams[v.ID] == v && !v.claimed {
			delete(t.streams, v.ID)
		}
		t.mu.Unlock()
	})
	return v, nil
}

// Removing the resource before closing the connection also rejects a late
// WebSocket upgrade that raced with cancellation while Accept was in progress.
func (t *session) closeStream(v *stream) {
	v.once.Do(func() {
		t.mu.Lock()
		delete(t.streams, v.ID)
		c := v.conn
		t.mu.Unlock()
		if c != nil {
			c.Close()
		}
		close(v.done)
	})
}
func (s *Server) createStream(w http.ResponseWriter, r *http.Request) {
	t := s.owned(w, r)
	if t == nil {
		return
	}
	var in struct {
		Kind string `json:"kind"`
		Rule string `json:"rule_id"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Kind != "adb" && in.Kind != "forward" || in.Kind == "forward" && in.Rule == "" || in.Kind == "adb" && in.Rule != "" {
		bad(w, 400, "invalid stream purpose")
		return
	}
	v, err := t.newStream(in.Kind, in.Rule)
	if err != nil {
		bad(w, 409, err.Error())
		return
	}
	w.Header().Set("Location", v.Path)
	reply(w, 201, v.Resource)
}
func (s *Server) data(w http.ResponseWriter, r *http.Request) {
	t := s.owned(w, r)
	if t == nil {
		return
	}
	t.mu.Lock()
	v := t.streams[r.PathValue("stream")]
	if t.closed || v == nil || v.claimed {
		t.mu.Unlock()
		bad(w, 404, "stream not found")
		return
	}
	v.claimed = true
	t.mu.Unlock()
	c, err := accept(w, r)
	defer t.closeStream(v)
	if err != nil {
		return
	}
	conn := binaryConn(t.ctx, c)
	t.mu.Lock()
	if t.closed || t.streams[v.ID] != v {
		t.mu.Unlock()
		c.CloseNow()
		return
	}
	v.conn = conn
	t.mu.Unlock()
	defer c.CloseNow()
	switch v.kind {
	case "adb":
		s.Gateway.Handle(t.ctx, conn, t.scope)
	case "forward":
		s.Gateway.Forwards.OpenForward(t.ctx, t.scope, v.rule, conn)
	case "reverse":
		select {
		case v.ready <- conn:
		case <-t.ctx.Done():
			return
		}
		select {
		case <-v.done:
		case <-t.ctx.Done():
		}
	}
}
func (t *session) Reverse(ctx context.Context, rule string, port int) (net.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, err := t.newStream("reverse", rule)
	if err != nil {
		return nil, err
	}
	transferred := false
	defer func() {
		if !transferred {
			t.closeStream(v)
		}
	}()
	if _, err = t.Request(ctx, gateway.Control{Op: "reverse", Rule: rule, Port: port, Stream: v.ID}); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case c := <-v.ready:
		transferred = true
		return &reverseConn{Conn: c, done: func() { t.closeStream(v) }}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-t.ctx.Done():
		return nil, errors.New("tunnel closed")
	case <-time.After(setupTimeout):
		return nil, fmt.Errorf("reverse stream attach timeout")
	}
}

type reverseConn struct {
	net.Conn
	done func()
	once sync.Once
}

func (c *reverseConn) Close() error { c.once.Do(c.done); return nil }
