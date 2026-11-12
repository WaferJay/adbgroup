package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
)

type inventoryClient interface {
	ListGroups(context.Context) ([]gateway.Group, error)
	ListDevices(context.Context, string) ([]gateway.Device, error)
}

type groupMarkers struct {
	Default, Current string
}

func runQuery(parent context.Context, client inventoryClient, command, group string, jsonOutput bool, out io.Writer, markers ...groupMarkers) error {
	ctx, cancel := context.WithTimeout(parent, 10*time.Second)
	defer cancel()
	if command == "groups" {
		groups, err := client.ListGroups(ctx)
		if err != nil {
			return err
		}
		if jsonOutput {
			return printJSON(out, groups)
		}
		w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
		if _, err := fmt.Fprintln(w, "NAME\tDEVICES\tDEFAULT\tCURRENT"); err != nil {
			return err
		}
		m := groupMarkers{Default: group}
		if len(markers) > 0 {
			m = markers[0]
		}
		for _, g := range groups {
			selected, current := "", ""
			if g.Name == m.Default {
				selected = "*"
			}
			if g.Name == m.Current {
				current = "*"
			}
			if _, err := fmt.Fprintf(w, "%s\t%d\t%s\t%s\n", g.Name, len(g.Devices), selected, current); err != nil {
				return err
			}
		}
		return w.Flush()
	}
	devices, err := client.ListDevices(ctx, group)
	if err != nil {
		return err
	}
	if jsonOutput {
		return printJSON(out, devices)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "SERIAL\tSTATE\tREADERS\tQUEUED\tWRITER\tDUPLICATE"); err != nil {
		return err
	}
	for _, d := range devices {
		writer := d.Writer
		if writer == "" {
			writer = "-"
		}
		if _, err := fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%s\t%t\n", d.Serial, d.State, d.Readers, d.Queued, writer, d.Duplicate); err != nil {
			return err
		}
	}
	return w.Flush()
}

func printJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
