package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const ProtocolVersion = 2

type Request struct {
	Version   int     `json:"version"`
	Operation string  `json:"operation"`
	Config    *Config `json:"config,omitempty"`
	ID        string  `json:"id,omitempty"`
}

type Response struct {
	Version int      `json:"version"`
	PID     int      `json:"pid"`
	Tunnel  *Tunnel  `json:"tunnel,omitempty"`
	Tunnels []Tunnel `json:"tunnels,omitempty"`
	Error   string   `json:"error,omitempty"`
}

func SocketPath(dir string) string { return filepath.Join(dir, "control.sock") }

// Call sends one request without starting a daemon as a side effect.
func Call(ctx context.Context, dir string, req Request) (Response, error) {
	var res Response
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", SocketPath(dir))
	if err != nil {
		return res, err
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	deadline := time.Now().Add(45 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.SetDeadline(deadline)
	req.Version = ProtocolVersion
	if err = json.NewEncoder(c).Encode(req); err != nil {
		return res, err
	}
	if err = json.NewDecoder(io.LimitReader(c, 1<<20)).Decode(&res); err != nil {
		return res, err
	}
	if res.Version != ProtocolVersion {
		return res, fmt.Errorf("incompatible daemon protocol %d (client %d); stop the old daemon before retrying", res.Version, ProtocolVersion)
	}
	if res.Error != "" {
		return res, errors.New(res.Error)
	}
	return res, nil
}

// Serve owns the listener and waits for all tunnels and handlers before returning.
func Serve(parent context.Context, l net.Listener, m *Manager) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	go func() { <-ctx.Done(); l.Close() }()
	var wg sync.WaitGroup
	var acceptErr error
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() == nil {
				acceptErr = err
			}
			break
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer c.Close()
			c.SetDeadline(time.Now().Add(45 * time.Second))
			stopRead := context.AfterFunc(ctx, func() { c.Close() })
			defer stopRead()
			var req Request
			res := Response{Version: ProtocolVersion, PID: os.Getpid()}
			d := json.NewDecoder(io.LimitReader(c, 64<<10))
			d.DisallowUnknownFields()
			if err := d.Decode(&req); err != nil {
				res.Error = err.Error()
				json.NewEncoder(c).Encode(res)
				return
			}
			if req.Version != ProtocolVersion {
				res.Error = "incompatible daemon protocol"
				json.NewEncoder(c).Encode(res)
				return
			}
			requestCtx, done := context.WithTimeout(ctx, 40*time.Second)
			defer done()
			// A client that exits while setup is pending must not leave a
			// starting tunnel behind. The protocol sends exactly one request.
			go func() {
				var extra [1]byte
				c.Read(extra[:])
				done()
			}()
			var opErr error
			switch req.Operation {
			case "status":
			case "list":
				res.Tunnels = m.List()
			case "create", "ensure":
				if req.Config == nil {
					opErr = errors.New("missing tunnel configuration")
					break
				}
				var info Tunnel
				var err error
				if req.Operation == "ensure" {
					info, err = m.Ensure(requestCtx, *req.Config)
				} else {
					info, err = m.Start(requestCtx, *req.Config)
				}
				res.Tunnel, opErr = &info, err
			case "stop":
				opErr = m.Stop(requestCtx, req.ID)
			case "shutdown":
				m.Close()
			default:
				opErr = fmt.Errorf("unknown daemon operation %q", req.Operation)
			}
			if opErr != nil {
				res.Error = opErr.Error()
			}
			json.NewEncoder(c).Encode(res)
			if req.Operation == "shutdown" {
				cancel()
			}
		}()
	}
	cancel()
	m.Close()
	wg.Wait()
	return acceptErr
}
