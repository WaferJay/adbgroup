package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/WaferJay/adbgroup/internal/daemon"
	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/tunnel"
)

const shellStateFile = "ADBGROUP_SHELL_STATE_FILE"

type selection struct {
	Config daemon.Config `json:"config"`
	ID     string        `json:"id"`
}

func currentSelection() *selection {
	var s selection
	if json.Unmarshal([]byte(os.Getenv("ADBGROUP_CONTEXT")), &s) != nil || s.ID == "" || s.Config.Group == "" || s.Config.Lease != "" {
		return nil
	}
	if s.Config.Normalize() != nil || os.Getenv("ADB_SERVER_SOCKET") != "tcp:"+s.Config.Listen {
		return nil
	}
	return &s
}

func connectionFlagsSet(flags *flag.FlagSet) bool {
	return flagWasSet(flags, "config", "f", "server", "s", "credentials", "d", "cert", "key", "ca")
}

func applySelection(o *tunnelOptions, flags *flag.FlagSet) *selection {
	s := currentSelection()
	if s == nil || connectionFlagsSet(flags) {
		return nil
	}
	o.server, o.cert, o.key, o.ca = s.Config.Server, s.Config.Cert, s.Config.Key, s.Config.CA
	if !flagWasSet(flags, "group", "g") {
		o.group = s.Config.Group
	}
	return s
}

func requireShell() error {
	if os.Getenv(shellStateFile) == "" {
		return errors.New("initialize shell integration with 'source <(command adg shell-init zsh)' (use bash instead of zsh for bash), or run 'adg --help' for usage")
	}
	return nil
}

// The shell owns this private file. No returned data is evaluated as shell code.
func writeShellState(action string, s *selection) error {
	f, err := os.OpenFile(os.Getenv(shellStateFile), os.O_WRONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
		return errors.New("shell state file must be private and regular")
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	socket, group, data := "", "", []byte{}
	if s != nil {
		socket = "tcp:" + s.Config.Listen
		group = s.Config.Group
		data, err = json.Marshal(s)
		if err != nil {
			return err
		}
	}
	_, err = fmt.Fprintf(f, "%s\n%s\n%s\n%s\n", action, socket, data, group)
	if err != nil {
		return err
	}
	return f.Close()
}

func runUnuse(args []string) error {
	flags := newCommandFlags("unuse")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unuse takes no arguments")
	}
	if err := requireShell(); err != nil {
		return err
	}
	return writeShellState("unset", nil)
}

func runUse(args []string, out io.Writer) error {
	flags := newCommandFlags("use")
	o := bindClientFlags(flags)
	path, err := defaultTunnelConfig()
	if err != nil {
		return err
	}
	file := flags.String("config", path, "client configuration file")
	flags.StringVar(file, "f", path, "short for -config")
	if err := parseClientGroupArgs(flags, args); err != nil {
		return err
	}
	if err := requireShell(); err != nil {
		return err
	}
	return o.selectTerminalGroup(o.group, *file, flags, out)
}

func (o *tunnelOptions) selectTerminalGroup(group, file string, flags *flag.FlagSet, out io.Writer) error {
	if err := o.loadConfig(file, flags); err != nil {
		return err
	}
	active := applySelection(o, flags)
	cert, key, ca := o.credentialFiles()
	client, err := tunnel.NewClient(o.server, cert, key, ca)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	if group == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		groups, err := client.ListGroups(ctx)
		cancel()
		if err != nil {
			return err
		}
		current := ""
		if active != nil {
			current = active.Config.Group
		}
		group, err = chooseGroup(groups, current, out)
		if err != nil {
			return err
		}
	}
	return o.activateTerminalGroup(group, out)
}

func (o *tunnelOptions) activateTerminalGroup(group string, out io.Writer) error {
	s, err := o.ensureGroupTunnel(group)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "Using group %s: %s (tunnel %s)\n", group, s.Config.Listen, s.ID); err != nil {
		return err
	}
	return writeShellState("set", s)
}

func (o tunnelOptions) ensureGroupTunnel(group string) (*selection, error) {
	// Ensure chooses the port. Normalize still needs a valid loopback address.
	o.group, o.listen, o.lease = group, "127.0.0.1:1", ""
	cfg, err := o.daemonConfig()
	if err != nil {
		return nil, err
	}
	dir, err := daemon.StateDir()
	if err != nil {
		return nil, err
	}
	ctx, cancel := commandContext()
	defer cancel()
	if err := ensureDaemon(ctx, dir); err != nil {
		return nil, err
	}
	res, err := daemon.Call(ctx, dir, daemon.Request{Operation: "ensure", Config: &cfg})
	if err != nil {
		return nil, err
	}
	if res.Tunnel == nil || res.Tunnel.State != "active" || res.Tunnel.Group != group {
		return nil, errors.New("daemon returned no active tunnel for the requested group")
	}
	cfg.Listen = res.Tunnel.Listen
	if err := cfg.Normalize(); err != nil {
		return nil, err
	}
	return &selection{Config: cfg, ID: res.Tunnel.ID}, nil
}

func chooseGroup(groups []gateway.Group, current string, out io.Writer) (string, error) {
	if len(groups) == 0 {
		return "", errors.New("client identity has no authorized groups")
	}
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		return "", errors.New("interactive terminal required; specify a group with adg use GROUP")
	}
	defer tty.Close()
	sort.Slice(groups, func(i, j int) bool { return groups[i].Name < groups[j].Name })
	for i, g := range groups {
		mark := ""
		if g.Name == current {
			mark = " (current)"
		}
		fmt.Fprintf(out, "%d) %s — %d devices%s\n", i+1, g.Name, len(g.Devices), mark)
	}
	fmt.Fprint(tty, "Choose group (number, Enter/q to cancel): ")
	// Keep default signal handling while waiting for human input: Ctrl-C exits
	// promptly, without a goroutine blocked on a terminal read or a menu timeout.
	line, err := bufio.NewReader(tty).ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read selection: %w", err)
	}
	return parseGroupChoice(groups, line)
}

func parseGroupChoice(groups []gateway.Group, line string) (string, error) {
	line = strings.TrimSpace(line)
	if line == "" || line == "q" {
		return "", errors.New("group selection cancelled")
	}
	n, err := strconv.Atoi(line)
	if err != nil || n < 1 || n > len(groups) {
		return "", errors.New("invalid group number")
	}
	return groups[n-1].Name, nil
}
