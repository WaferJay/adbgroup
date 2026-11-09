package gateway

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestMonitorUSBBackendFormats(t *testing.T) {
	for _, tc := range []struct {
		name, listing string
		usb           bool
	}{
		{"native", "A device usb:1-2 product:pixel transport_id:1\n", true},
		{"libusb", "A device 1-2 product:pixel transport_id:1\n", true},
		{"libusb_hub", "A device 2-4.5.6 product:pixel transport_id:1\n", true},
		{"older_usb", "A device usb:1:2 transport_id:1\n", true},
		{"unauthorized", "A unauthorized 1-2 transport_id:1\n", true},
		{"no_permissions", "A no permissions (user test); 1-2 transport_id:1\n", true},
		{"tcp", "A device product:pixel model:Pixel transport_id:1\n", false},
		{"metadata_is_not_path", "A device product:1-2 model:usb:1-2 transport_id:1\n", false},
		{"serial_is_not_path", "1-2 device product:pixel transport_id:1\n", false},
		{"emulator", "emulator-5554 device product:sdk transport_id:1\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := NewEngine(testConfig())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			b := Backend{Dial: func(context.Context) (net.Conn, error) {
				a, b := net.Pipe()
				go func() {
					defer b.Close()
					if _, err := ReadFrame(b); err != nil {
						return
					}
					if okay(b) != nil {
						return
					}
					WriteFrame(b, tc.listing)
					<-ctx.Done()
				}()
				return a, nil
			}}
			changed := e.Changed()
			done := make(chan struct{})
			go func() { b.Monitor(ctx, e); close(done) }()
			defer func() { cancel(); <-done }()
			select {
			case <-changed:
			case <-time.After(time.Second):
				t.Fatal("monitor did not update")
			}
			e.mu.Lock()
			count := len(e.devices)
			e.mu.Unlock()
			if (count == 1) != tc.usb {
				t.Fatalf("USB device count=%d for %q; want USB=%t", count, tc.listing, tc.usb)
			}
		})
	}
}
