package gateway

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

func ReadFrame(r io.Reader) (string, error) {
	var h [4]byte
	if _, e := io.ReadFull(r, h[:]); e != nil {
		return "", e
	}
	n, e := strconv.ParseUint(string(h[:]), 16, 16)
	if e != nil {
		return "", e
	}
	b := make([]byte, int(n))
	_, e = io.ReadFull(r, b)
	return string(b), e
}
func WriteFrame(w io.Writer, s string) error {
	if len(s) > 65535 {
		return errors.New("ADB frame too large")
	}
	return writeBytes(w, []byte(fmt.Sprintf("%04x%s", len(s), s)))
}
func writeBytes(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, e := w.Write(b)
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
func okay(w io.Writer) error    { return writeBytes(w, []byte("OKAY")) }
func fail(w io.Writer, e error) { writeBytes(w, []byte("FAIL")); WriteFrame(w, e.Error()) }
func readStatus(r io.Reader) error {
	var b [4]byte
	if _, e := io.ReadFull(r, b[:]); e != nil {
		return e
	}
	if string(b[:]) == "OKAY" {
		return nil
	}
	if string(b[:]) == "FAIL" {
		s, e := ReadFrame(r)
		if e != nil {
			return e
		}
		return errors.New(s)
	}
	return fmt.Errorf("invalid ADB status %q", b)
}

type Backend struct {
	Address string
	Dial    func(context.Context) (net.Conn, error)
}

func (b Backend) Open(ctx context.Context, request string) (net.Conn, error) {
	var c net.Conn
	var e error
	if b.Dial != nil {
		c, e = b.Dial(ctx)
	} else {
		c, e = (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", b.Address)
	}
	if e != nil {
		return nil, e
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if e = WriteFrame(c, request); e == nil {
		e = readStatus(c)
	}
	if e != nil {
		c.Close()
		return nil, e
	}
	c.SetDeadline(time.Time{})
	return c, nil
}
func (b Backend) Query(ctx context.Context, request string) (string, error) {
	c, e := b.Open(ctx, request)
	if e != nil {
		return "", e
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	return ReadFrame(c)
}
func (b Backend) Device(ctx context.Context, d Device, request string) (net.Conn, error) {
	if d.TransportID == "" {
		return nil, errors.New("backend did not report transport_id; recent Platform Tools required")
	}
	c, e := b.Open(ctx, "host:transport-id:"+d.TransportID)
	if e != nil {
		return nil, e
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if e = WriteFrame(c, request); e == nil {
		e = readStatus(c)
	}
	if e != nil {
		c.Close()
		return nil, e
	}
	c.SetDeadline(time.Time{})
	return c, nil
}
func ParseDevices(s string) []Device {
	var out []Device
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		d := Device{Serial: fields[0], State: fields[1]}
		for _, f := range fields[2:] {
			if strings.HasPrefix(f, "transport_id:") {
				d.TransportID = strings.TrimPrefix(f, "transport_id:")
			}
		}
		if len(fields) > 2 {
			d.Detail = strings.Join(fields[2:], " ")
		}
		out = append(out, d)
	}
	return out
}
func (b Backend) Monitor(ctx context.Context, e *Engine) {
	for ctx.Err() == nil {
		c, err := b.Open(ctx, "host:track-devices-l")
		if err == nil {
			done := make(chan struct{})
			go func() {
				select {
				case <-ctx.Done():
					c.Close()
				case <-done:
				}
			}()
			for {
				s, x := ReadFrame(c)
				if x != nil {
					err = x
					break
				}
				var usb []Device
				for _, d := range ParseDevices(s) {
					if strings.Contains(" "+d.Detail, " usb:") {
						usb = append(usb, d)
					}
				}
				e.Update(usb)
			}
			close(done)
			c.Close()
		}
		e.Update(nil)
		if ctx.Err() != nil {
			return
		}
		slog.Warn("backend device subscription disconnected", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// pumpConn detects a waiting client's disconnect without consuming protocol bytes.
type pumpConn struct {
	net.Conn
	r      *io.PipeReader
	cancel context.CancelFunc
}

func watch(ctx context.Context, c net.Conn) (context.Context, *pumpConn) {
	ctx, cancel := context.WithCancel(ctx)
	r, w := io.Pipe()
	p := &pumpConn{c, r, cancel}
	go func() { _, err := io.Copy(w, c); w.CloseWithError(err); cancel() }()
	go func() { <-ctx.Done(); p.Close() }()
	return ctx, p
}
func (p *pumpConn) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *pumpConn) Close() error               { p.cancel(); p.r.Close(); return p.Conn.Close() }
func bridge(a, b io.ReadWriteCloser) {
	done := make(chan struct{})
	go func() { io.Copy(a, b); a.Close(); b.Close(); close(done) }()
	io.Copy(b, a)
	a.Close()
	b.Close()
	<-done
}

type Gateway struct {
	Engine   *Engine
	Backend  Backend
	Forwards *ForwardManager
}

func NewGateway(e *Engine, b Backend) *Gateway {
	g := &Gateway{Engine: e, Backend: b}
	g.Forwards = NewForwardManager(g)
	return g
}
func (g *Gateway) Serve(ctx context.Context, l net.Listener, s Scope) {
	go func() { <-ctx.Done(); l.Close() }()
	for {
		c, e := l.Accept()
		if e != nil {
			return
		}
		go g.Handle(ctx, c, s)
	}
}
func (g *Gateway) Handle(parent context.Context, raw net.Conn, s Scope) {
	if !g.Engine.AuthorizedScope(s) {
		raw.Close()
		return
	}
	ctx, c := watch(parent, raw)
	defer c.Close()
	var selected *Device
	var release func()
	defer func() {
		if release != nil {
			release()
		}
	}()
	for {
		if selected == nil {
			c.SetReadDeadline(time.Now().Add(30 * time.Second))
		}
		req, err := ReadFrame(c)
		if err != nil {
			return
		}
		c.SetReadDeadline(time.Time{})
		if selected != nil {
			// Modern adb force-selects a transport before host forwarding commands.
			if strings.HasPrefix(req, "host:forward:") {
				g.Forwards.Forward(ctx, c, s, *selected, strings.TrimPrefix(req, "host:forward:"))
				return
			}
			if strings.HasPrefix(req, "host:killforward:") {
				g.Forwards.Remove(c, s, *selected, strings.TrimPrefix(req, "host:killforward:"))
				return
			}
			if req == "host:killforward-all" {
				g.Forwards.RemoveAll(ctx, c, s, selected)
				return
			}
			if strings.HasPrefix(req, "reverse:") {
				g.Forwards.Reverse(ctx, c, s, *selected, strings.TrimPrefix(req, "reverse:"))
				return
			}
			if !allowedService(req) {
				fail(c, errors.New("device service not supported"))
				return
			}
			up, err := g.Backend.Device(ctx, *selected, req)
			if err != nil {
				fail(c, err)
				return
			}
			defer up.Close()
			if okay(c) != nil {
				return
			}
			bridge(c, up)
			return
		}
		switch req {
		case "host:version":
			g.query(ctx, c, "host:version")
			return
		case "host:host-features":
			g.query(ctx, c, req)
			return
		case "host:devices", "host:devices-l":
			okay(c)
			WriteFrame(c, g.deviceList(s, req == "host:devices-l"))
			return
		case "host:track-devices", "host:track-devices-l":
			okay(c)
			last := "\x00"
			for {
				ch := g.Engine.Changed()
				list := g.deviceList(s, req == "host:track-devices-l")
				if list != last {
					if WriteFrame(c, list) != nil {
						return
					}
					last = list
				}
				select {
				case <-ctx.Done():
					return
				case <-ch:
				}
			}
		case "host:list-forward":
			g.Forwards.List(c, s)
			return
		case "host:killforward-all":
			g.Forwards.RemoveAll(ctx, c, s, nil)
			return
		}
		serial, id, kind, command, transport, tport, err := parseRequest(req)
		if err != nil {
			fail(c, err)
			return
		}
		d, err := g.Engine.Select(s, serial, id, kind)
		if err != nil {
			fail(c, err)
			return
		}
		acq, cancel := context.WithTimeout(ctx, g.Engine.Config().ReadWait())
		release, err = g.Engine.Acquire(acq, s, d, c)
		cancel()
		if err != nil {
			fail(c, err)
			return
		}
		slog.Info("device access", "identity", s.Identity, "group", s.Group, "device", d.Serial, "tunnel", s.Tunnel, "lease", s.Lease)
		if transport {
			selected = &d
			if okay(c) != nil {
				return
			}
			if tport {
				tid, e := strconv.ParseUint(d.TransportID, 10, 64)
				if e != nil {
					return
				}
				var buf [8]byte
				binary.LittleEndian.PutUint64(buf[:], tid)
				if writeBytes(c, buf[:]) != nil {
					return
				}
			}
			continue
		}
		switch command {
		case "get-state":
			okay(c)
			WriteFrame(c, d.State)
		case "get-serialno":
			okay(c)
			WriteFrame(c, d.Serial)
		case "get-devpath":
			okay(c)
			WriteFrame(c, d.Detail)
		case "features":
			g.query(ctx, c, "host-transport-id:"+d.TransportID+":features")
		case "list-forward":
			g.Forwards.List(c, s)
		case "killforward-all":
			g.Forwards.RemoveAll(ctx, c, s, &d)
		default:
			if strings.HasPrefix(command, "forward:") {
				g.Forwards.Forward(ctx, c, s, d, strings.TrimPrefix(command, "forward:"))
			} else if strings.HasPrefix(command, "killforward:") {
				g.Forwards.Remove(c, s, d, strings.TrimPrefix(command, "killforward:"))
			} else {
				fail(c, errors.New("host command not supported"))
			}
		}
		return
	}
}
func (g *Gateway) query(ctx context.Context, c io.Writer, req string) {
	s, e := g.Backend.Query(ctx, req)
	if e != nil {
		fail(c, e)
		return
	}
	okay(c)
	WriteFrame(c, s)
}
func (g *Gateway) deviceList(s Scope, long bool) string {
	var b strings.Builder
	for _, d := range g.Engine.Snapshot(s) {
		state := d.State
		if d.Duplicate {
			state = "offline"
		}
		fmt.Fprintf(&b, "%s\t%s", d.Serial, state)
		if long && d.Detail != "" {
			fmt.Fprintf(&b, " %s", d.Detail)
		}
		b.WriteByte('\n')
	}
	return b.String()
}
func parseRequest(req string) (serial, id, kind, cmd string, transport, tport bool, err error) {
	err = errors.New("host command not supported")
	if strings.HasPrefix(req, "host:tport:") {
		tport = true
		transport = true
		rest := strings.TrimPrefix(req, "host:tport:")
		if strings.HasPrefix(rest, "serial:") {
			serial = strings.TrimPrefix(rest, "serial:")
		} else if rest == "any" || rest == "usb" || rest == "local" {
			kind = rest
		} else {
			return
		}
		err = nil
		return
	}
	if strings.HasPrefix(req, "host:transport-id:") {
		id = strings.TrimPrefix(req, "host:transport-id:")
		transport = true
		err = nil
		return
	}
	if strings.HasPrefix(req, "host:transport:") {
		serial = strings.TrimPrefix(req, "host:transport:")
		transport = true
		err = nil
		return
	}
	for _, k := range []string{"any", "usb", "local"} {
		if req == "host:transport-"+k {
			kind = k
			transport = true
			err = nil
			return
		}
	}
	for _, p := range []string{"host-serial:", "host-transport-id:"} {
		if strings.HasPrefix(req, p) {
			parts := strings.SplitN(strings.TrimPrefix(req, p), ":", 2)
			if len(parts) != 2 || parts[0] == "" {
				return
			}
			if p == "host-serial:" {
				serial = parts[0]
			} else {
				id = parts[0]
			}
			cmd = parts[1]
			err = nil
			return
		}
	}
	for _, p := range []string{"host:", "host-usb:", "host-local:"} {
		if strings.HasPrefix(req, p) {
			cmd = strings.TrimPrefix(req, p)
			if p == "host-local:" {
				kind = "local"
			}
			err = nil
			return
		}
	}
	return
}
func allowedService(req string) bool {
	for _, p := range []string{"shell:", "shell,", "exec:", "sync:", "abb:", "abb_exec:", "reboot:", "jdwp:", "tcp:", "localabstract:"} {
		if strings.HasPrefix(req, p) {
			return true
		}
	}
	return req == "track-jdwp" || req == "track-app"
}
