package tunnel

import (
	"adbgroup/internal/gateway"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type Client struct {
	base        *url.URL
	http        *http.Client
	mu          sync.Mutex
	listeners   map[string]*localListener
	reverse     map[string]int
	connections map[net.Conn]struct{}
	listen      func(string, string) (net.Listener, error)
	dial        func(context.Context, string) (net.Conn, error)
	path        string
	ctx         context.Context
}
type localListener struct {
	listener net.Listener
	rule     string
	started  bool
}

func NewClient(server, certFile, keyFile, caFile string) (*Client, error) {
	u, err := url.Parse(server)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" {
		return nil, errors.New("server must be an HTTPS origin")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("no server CA certificates")
	}
	tr := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{cert}, NextProtos: []string{"http/1.1"}}, TLSHandshakeTimeout: setupTimeout, ResponseHeaderTimeout: setupTimeout}
	return &Client{base: u, http: &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") }}, listeners: map[string]*localListener{}, reverse: map[string]int{}, connections: map[net.Conn]struct{}{}, listen: net.Listen, dial: func(ctx context.Context, a string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", a)
	}}, nil
}
func (c *Client) endpoint(path string, ws bool) string {
	u := *c.base
	u.Path = ""
	u.RawPath = ""
	v, _ := url.Parse(path)
	u.Path = v.Path
	u.RawPath = v.RawPath
	if ws {
		u.Scheme = "wss"
	}
	return u.String()
}
func (c *Client) request(ctx context.Context, method, path string, in, out any) error {
	var b bytes.Buffer
	if in != nil {
		if err := json.NewEncoder(&b).Encode(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path, false), &b)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		v, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		return fmt.Errorf("%s: %s", res.Status, strings.TrimSpace(string(v)))
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(out)
	}
	return nil
}
func (c *Client) websocket(ctx context.Context, path string) (*websocket.Conn, error) {
	v, res, err := websocket.Dial(ctx, c.endpoint(path, true), &websocket.DialOptions{HTTPClient: c.http, CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		if res != nil && res.Body != nil {
			res.Body.Close()
		}
		return nil, err
	}
	v.SetReadLimit(64 << 10)
	return v, nil
}
func localAddress(address string) error {
	h, p, e := net.SplitHostPort(address)
	if e != nil {
		return e
	}
	ip := net.ParseIP(h)
	n, e := strconv.Atoi(p)
	if ip == nil || !ip.IsLoopback() || e != nil || n < 1 || n > 65535 {
		return errors.New("listen must be a numeric loopback address with port 1..65535")
	}
	return nil
}

// Run owns the local ADB listener and every associated data socket. It never
// reconnects or replays commands after the authenticated control channel fails.
func (c *Client) Run(parent context.Context, group, lease, address string) error {
	if err := localAddress(address); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	c.ctx = ctx
	var resource Resource
	setup, stop := context.WithTimeout(ctx, setupTimeout)
	err := c.request(setup, "POST", "/v1/groups/"+url.PathEscape(group)+"/tunnels", map[string]string{"lease_id": lease}, &resource)
	stop()
	if err != nil {
		return err
	}
	c.path = resource.Path
	defer func() {
		c.shutdown()
		cleanup, done := context.WithTimeout(context.Background(), setupTimeout)
		defer done()
		c.request(cleanup, "DELETE", c.path, nil, nil)
		c.http.CloseIdleConnections()
	}()
	control, err := c.websocket(ctx, c.path+"/control")
	if err != nil {
		return err
	}
	defer control.CloseNow()
	setup, stop = context.WithTimeout(ctx, setupTimeout)
	var ready gateway.Control
	err = wsjson.Read(setup, control, &ready)
	stop()
	if err != nil {
		return err
	}
	if ready.Version != 1 || ready.Op != "ready" {
		return errors.New("incompatible tunnel protocol")
	}
	l, err := c.listen("tcp", address)
	if err != nil {
		return err
	}
	defer l.Close()
	go func() { <-ctx.Done(); l.Close(); control.CloseNow(); c.shutdown() }()
	go func() {
		for {
			raw, e := l.Accept()
			if e != nil {
				return
			}
			if !c.track(raw) {
				raw.Close()
				continue
			}
			go c.pipeLocal(raw, "adb", "")
		}
	}()
	slog.Info("ADB tunnel ready", "listen", address, "group", group, "lease", lease)
	slots := make(chan struct{}, 64)
	for {
		var m gateway.Control
		if err = wsjson.Read(ctx, control, &m); err != nil {
			if parent.Err() != nil {
				return nil
			}
			return fmt.Errorf("control disconnected; forward/reverse rules cleared: %w", err)
		}
		if m.Version != 1 || m.ID == "" {
			return errors.New("invalid control request")
		}
		select {
		case slots <- struct{}{}:
		default:
			return errors.New("control queue full")
		}
		go func(m gateway.Control) {
			defer func() { <-slots }()
			result := c.handle(m)
			result.Version = 1
			result.ID = m.ID
			result.Op = "reply"
			writeCtx, done := context.WithTimeout(ctx, setupTimeout)
			defer done()
			if wsjson.Write(writeCtx, control, result) != nil {
				cancel()
			}
		}(m)
	}
}
func (c *Client) track(conn net.Conn) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil || len(c.connections) >= 128 {
		return false
	}
	c.connections[conn] = struct{}{}
	return true
}
func (c *Client) untrack(conn net.Conn) { c.mu.Lock(); delete(c.connections, conn); c.mu.Unlock() }
func (c *Client) shutdown() {
	c.mu.Lock()
	var conns []net.Conn
	var listeners []net.Listener
	for v := range c.connections {
		conns = append(conns, v)
	}
	for _, v := range c.listeners {
		listeners = append(listeners, v.listener)
	}
	c.listeners = map[string]*localListener{}
	c.reverse = map[string]int{}
	c.mu.Unlock()
	for _, l := range listeners {
		l.Close()
	}
	for _, v := range conns {
		v.Close()
	}
}
func (c *Client) open(kind, rule string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(c.ctx, setupTimeout)
	defer cancel()
	var v Resource
	if err := c.request(ctx, "POST", c.path+"/streams", map[string]string{"kind": kind, "rule_id": rule}, &v); err != nil {
		return nil, err
	}
	w, err := c.websocket(ctx, v.Path)
	if err != nil {
		return nil, err
	}
	return binaryConn(c.ctx, w), nil
}
func (c *Client) pipeLocal(raw net.Conn, kind, rule string) {
	defer c.untrack(raw)
	defer raw.Close()
	up, err := c.open(kind, rule)
	if err != nil {
		return
	}
	copyBoth(raw, up)
}
func copyBoth(a, b net.Conn) {
	done := make(chan struct{})
	go func() { io.Copy(a, b); a.Close(); b.Close(); close(done) }()
	io.Copy(b, a)
	a.Close()
	b.Close()
	<-done
}
func (c *Client) handle(m gateway.Control) gateway.Control {
	result := gateway.Control{}
	fail := func(e error) gateway.Control { return gateway.Control{Error: e.Error()} }
	switch m.Op {
	case "bind":
		if m.Rule == "" || m.Port < 0 || m.Port > 65535 {
			return fail(errors.New("invalid bind"))
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.ctx.Err() != nil {
			return fail(c.ctx.Err())
		}
		if old := c.listeners[m.Previous]; m.Previous != "" && old != nil {
			if old.listener.Addr().(*net.TCPAddr).Port != m.Port {
				return fail(errors.New("rebind port mismatch"))
			}
			c.listeners[m.Rule] = old
			result.Port = m.Port
			return result
		}
		if len(c.listeners) >= 128 {
			return fail(errors.New("forward rule limit reached"))
		}
		l, err := c.listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(m.Port)))
		if err != nil {
			return fail(err)
		}
		c.listeners[m.Rule] = &localListener{listener: l}
		result.Port = l.Addr().(*net.TCPAddr).Port
	case "activate":
		c.mu.Lock()
		defer c.mu.Unlock()
		v := c.listeners[m.Rule]
		if v == nil {
			return fail(errors.New("listener not found"))
		}
		for k, other := range c.listeners {
			if other == v && k != m.Rule {
				delete(c.listeners, k)
			}
		}
		v.rule = m.Rule
		if !v.started {
			v.started = true
			go c.serveForward(v)
		}
	case "remove":
		c.mu.Lock()
		v := c.listeners[m.Rule]
		delete(c.listeners, m.Rule)
		closeListener := v != nil && (v.rule == m.Rule || v.rule == "")
		c.mu.Unlock()
		if closeListener {
			v.listener.Close()
		}
	case "remember":
		if m.Rule == "" || m.Port < 1 || m.Port > 65535 {
			return fail(errors.New("invalid reverse target"))
		}
		c.mu.Lock()
		if c.ctx.Err() != nil {
			c.mu.Unlock()
			return fail(c.ctx.Err())
		}
		if len(c.reverse) >= 128 {
			c.mu.Unlock()
			return fail(errors.New("reverse rule limit reached"))
		}
		c.reverse[m.Rule] = m.Port
		c.mu.Unlock()
	case "forget":
		c.mu.Lock()
		delete(c.reverse, m.Rule)
		c.mu.Unlock()
	case "reverse":
		c.mu.Lock()
		port, ok := c.reverse[m.Rule]
		c.mu.Unlock()
		if !ok || port != m.Port || m.Stream == "" {
			return fail(errors.New("unknown reverse rule"))
		}
		ctx, cancel := context.WithTimeout(c.ctx, setupTimeout)
		defer cancel()
		raw, err := c.dial(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return fail(err)
		}
		if !c.track(raw) {
			raw.Close()
			return fail(errors.New("client busy or closed"))
		}
		ws, err := c.websocket(ctx, c.path+"/streams/"+url.PathEscape(m.Stream))
		if err != nil {
			c.untrack(raw)
			raw.Close()
			return fail(err)
		}
		up := binaryConn(c.ctx, ws)
		go func() { defer c.untrack(raw); copyBoth(raw, up) }()
	default:
		return fail(errors.New("unsupported control operation"))
	}
	return result
}
func (c *Client) serveForward(v *localListener) {
	for {
		raw, err := v.listener.Accept()
		if err != nil {
			return
		}
		if !c.track(raw) {
			raw.Close()
			continue
		}
		c.mu.Lock()
		rule := v.rule
		c.mu.Unlock()
		go c.pipeLocal(raw, "forward", rule)
	}
}

// binaryConn aborts the transport on Close; no graceful network handshake is
// allowed to delay device revocation or the lock engine's closing barrier.
type wireConn struct {
	net.Conn
	ws *websocket.Conn
}

func (c *wireConn) Close() error { c.ws.CloseNow(); return c.Conn.Close() }
func binaryConn(ctx context.Context, ws *websocket.Conn) net.Conn {
	return &wireConn{Conn: websocket.NetConn(ctx, ws, websocket.MessageBinary), ws: ws}
}
