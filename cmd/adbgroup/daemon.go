package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/WaferJay/adbgroup/internal/daemon"
)

func commandContext() (context.Context, context.CancelFunc) {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	timed, done := context.WithTimeout(ctx, 45*time.Second)
	return timed, func() { done(); cancel() }
}

func ensureDaemon(ctx context.Context, dir string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	startup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	return daemon.Ensure(startup, dir, executable)
}

func startManagedTunnel(o *tunnelOptions, cert, key, ca string, out io.Writer) error {
	cfg := daemon.Config{Server: o.server, Group: o.group, Lease: o.lease, Listen: o.listen}
	var err error
	for _, p := range []struct {
		target *string
		value  string
	}{{&cfg.Cert, cert}, {&cfg.Key, key}, {&cfg.CA, ca}} {
		*p.target, err = filepath.Abs(p.value)
		if err != nil {
			return err
		}
	}
	if err := cfg.Normalize(); err != nil {
		return err
	}
	dir, err := daemon.StateDir()
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	if err := ensureDaemon(ctx, dir); err != nil {
		return err
	}
	res, err := daemon.Call(ctx, dir, daemon.Request{Operation: "create", Config: &cfg})
	if err != nil {
		return err
	}
	if res.Tunnel == nil {
		return errors.New("daemon returned no tunnel")
	}
	_, err = fmt.Fprintf(out, "Tunnel %s ready: group=%s listen=%s\nexport ADB_SERVER_SOCKET=tcp:%s\n", res.Tunnel.ID, res.Tunnel.Group, res.Tunnel.Listen, res.Tunnel.Listen)
	return err
}

func runTunnelCommand(args []string, out io.Writer) error {
	flags := newCommandFlags("tunnel " + args[0])
	var jsonOutput bool
	if args[0] == "list" {
		flags.BoolVar(&jsonOutput, "json", false, "print JSON instead of a table")
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if args[0] == "stop" && flags.NArg() != 1 || args[0] == "list" && flags.NArg() != 0 {
		flags.Usage()
		return errors.New("unexpected positional arguments")
	}
	dir, err := daemon.StateDir()
	if err != nil {
		return err
	}
	ctx, cancel := commandContext()
	defer cancel()
	if args[0] == "stop" {
		_, err := daemon.Call(ctx, dir, daemon.Request{Operation: "stop", ID: flags.Arg(0)})
		return err
	}
	res, err := daemon.Call(ctx, dir, daemon.Request{Operation: "list"})
	if err != nil && !daemon.Unavailable(err) {
		return err
	}
	if res.Tunnels == nil {
		res.Tunnels = []daemon.Tunnel{}
	}
	if jsonOutput {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(res.Tunnels)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(w, "ID\tSERVER\tGROUP\tLISTEN\tSTATE\tERROR"); err != nil {
		return err
	}
	for _, t := range res.Tunnels {
		if _, err := fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Server, t.Group, t.Listen, t.State, t.Error); err != nil {
			return err
		}
	}
	return w.Flush()
}

func runDaemonCommand(args []string, out io.Writer) error {
	flags := newCommandFlags("daemon")
	if err := flags.Parse(args); err != nil {
		return err
	}
	args = flags.Args()
	if len(args) == 0 {
		flags.Usage()
		return errors.New("a daemon command is required")
	}
	switch args[0] {
	case "start", "status", "stop", "run":
	default:
		flags.Usage()
		return fmt.Errorf("unknown daemon command %q", args[0])
	}
	flags = newCommandFlags("daemon " + args[0])
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return errors.New("unexpected positional arguments")
	}
	dir, err := daemon.StateDir()
	if err != nil {
		return err
	}
	if args[0] == "run" {
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return daemon.Run(ctx, dir)
	}
	ctx, cancel := commandContext()
	defer cancel()
	switch args[0] {
	case "start":
		if err := ensureDaemon(ctx, dir); err != nil {
			return err
		}
	case "stop":
		_, err := daemon.Call(ctx, dir, daemon.Request{Operation: "shutdown"})
		if daemon.Unavailable(err) {
			return nil
		}
		if err != nil {
			return err
		}
		// The shutdown reply confirms tunnel cleanup; wait for the lifetime lock
		// and socket to be released before a subsequent start.
		return daemon.WaitStopped(ctx, dir)
	case "status":
	}
	res, err := daemon.Call(ctx, dir, daemon.Request{Operation: "status"})
	if daemon.Unavailable(err) {
		_, err = fmt.Fprintln(out, "daemon stopped")
		return err
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "daemon running (pid %d)\n", res.PID)
	return err
}
