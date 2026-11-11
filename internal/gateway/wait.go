package gateway

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

func parseWaitCommand(command string) (kind string, states []string, err error) {
	// The adb client expands wait-for-device to wait-for-<transport>-device.
	spec := strings.TrimPrefix(command, "wait-for-")
	parts := strings.Split(spec, "-")
	if len(parts) < 2 {
		return "", nil, fmt.Errorf("bad wait-for- command: %s", spec)
	}
	kind = parts[0]
	switch kind {
	case "any", "usb", "local":
	default:
		return "", nil, fmt.Errorf("bad wait-for- transport: %s", spec)
	}
	states = parts[1:]
	for _, state := range states {
		switch state {
		case "device", "recovery", "rescue", "sideload", "bootloader", "any", "disconnect":
		default:
			return "", nil, fmt.Errorf("bad wait-for- state: %s", spec)
		}
	}
	return kind, states, nil
}

func (g *Gateway) waitFor(ctx context.Context, c io.Writer, s Scope, serial, id, command string) {
	kind, states, err := parseWaitCommand(command)
	if err != nil {
		fail(c, err)
		return
	}
	// Host wait services acknowledge acceptance, then send a second status when
	// the condition is met. They observe state without acquiring a device lock.
	if okay(c) != nil {
		return
	}
	if err := g.Engine.waitFor(ctx, s, serial, id, kind, states); err != nil {
		fail(c, err)
		return
	}
	okay(c)
}

func (e *Engine) waitFor(ctx context.Context, s Scope, serial, id, kind string, states []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !e.authorizedScope(s) || serial != "" && !e.permitted(s, serial) {
			return errors.New("device not found or not authorized")
		}
		var match *Device
		for _, d := range e.devices {
			if !e.permitted(s, d.Serial) || kind == "local" {
				continue
			}
			if serial != "" && d.Serial != serial || id != "" && d.TransportID != id {
				continue
			}
			if match != nil || d.Duplicate {
				return errors.New("more than one device/emulator")
			}
			match = d
		}
		for _, state := range states {
			// USB disconnect requires disappearance, not merely an offline state.
			if state == "disconnect" && match == nil || match != nil && (state == "any" || state == match.State) {
				return nil
			}
		}
		// Subscribe while holding the same mutex used to read device state so
		// an arrival, removal, or permission reload cannot be missed.
		changed := e.changed
		e.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-changed:
		}
		e.mu.Lock()
	}
}
