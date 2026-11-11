package main

import (
	"adbgroup/internal/gateway"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type inventoryStub struct {
	groups   []gateway.Group
	devices  []gateway.Device
	selected string
	err      error
}

func (c *inventoryStub) ListGroups(context.Context) ([]gateway.Group, error) {
	return c.groups, c.err
}

func (c *inventoryStub) ListDevices(_ context.Context, group string) ([]gateway.Device, error) {
	c.selected = group
	return c.devices, c.err
}

func TestInventoryQueriesPrintTablesAndJSON(t *testing.T) {
	client := &inventoryStub{
		groups:  []gateway.Group{{Name: "team-a", Devices: []string{"A", "B"}}, {Name: "team-b", Devices: []string{}}},
		devices: []gateway.Device{{Serial: "A", State: "device", Readers: 2, Queued: 1, Writer: "lease-id"}, {Serial: "B", State: "disconnected"}},
	}
	for _, command := range []string{"groups", "devices"} {
		t.Run(command, func(t *testing.T) {
			var out bytes.Buffer
			if err := runQuery(context.Background(), client, command, "team-a", false, &out); err != nil {
				t.Fatal(err)
			}
			lines := strings.Split(strings.TrimSpace(out.String()), "\n")
			var wantRows [][]string
			if command == "groups" {
				wantRows = [][]string{{"NAME", "DEVICES", "DEFAULT"}, {"team-a", "2", "*"}, {"team-b", "0"}}
			} else {
				wantRows = [][]string{{"SERIAL", "STATE", "READERS", "QUEUED", "WRITER", "DUPLICATE"}, {"A", "device", "2", "1", "lease-id", "false"}, {"B", "disconnected", "0", "0", "-", "false"}}
				if client.selected != "team-a" {
					t.Fatalf("queried group = %q", client.selected)
				}
			}
			if len(lines) != len(wantRows) {
				t.Fatalf("unexpected table: %s", out.String())
			}
			for i, want := range wantRows {
				if !reflect.DeepEqual(strings.Fields(lines[i]), want) {
					t.Errorf("table row %d = %q, want %v", i, lines[i], want)
				}
			}
			out.Reset()
			if err := runQuery(context.Background(), client, command, "team-a", true, &out); err != nil {
				t.Fatal(err)
			}
			if command == "groups" {
				var got []gateway.Group
				if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, client.groups) {
					t.Fatalf("invalid groups JSON: %s (%v)", out.String(), err)
				}
			} else {
				var got []gateway.Device
				if err := json.Unmarshal(out.Bytes(), &got); err != nil || !reflect.DeepEqual(got, client.devices) {
					t.Fatalf("invalid devices JSON: %s (%v)", out.String(), err)
				}
			}
		})
	}
}

func TestInventoryQueryErrorsLeaveStdoutEmpty(t *testing.T) {
	want := errors.New("403 Forbidden")
	client := &inventoryStub{err: want}
	for _, command := range []string{"groups", "devices"} {
		for _, jsonOutput := range []bool{false, true} {
			var out bytes.Buffer
			if err := runQuery(context.Background(), client, command, "team-b", jsonOutput, &out); !errors.Is(err, want) {
				t.Errorf("query error = %v, want %v", err, want)
			}
			if out.Len() != 0 {
				t.Errorf("failed query printed output: %s", out.String())
			}
		}
	}
}

func TestInventoryCommandsLoadDefaultClientConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	path, err := defaultTunnelConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// An invalid HTTPS origin must be rejected before reading credentials or
	// making requests, proving these commands used the default client config.
	if err = os.WriteFile(path, []byte("server: http://invalid.example\ngroup: team-a\n"), 0600); err != nil {
		t.Fatal(err)
	}
	previous := os.Args
	defer func() { os.Args = previous }()
	for _, command := range []string{"groups", "g", "devices", "d"} {
		os.Args = []string{"adbgroup", command, "--json"}
		if err = run(); err == nil || err.Error() != "server must be an HTTPS origin" {
			t.Errorf("%s did not use client config and query flags: %v", command, err)
		}
	}
}
