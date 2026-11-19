package main

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// Parse connection options one at a time: either GROUP or -g GROUP ends
// adg's options immediately, even when adb's first argument is another flag.
func parseGroupArgs(flags *flag.FlagSet, o *tunnelOptions, args []string) (string, []string, error) {
	for len(args) > 0 {
		arg := args[0]
		if !strings.HasPrefix(arg, "-") {
			if _, reserved := commandHelp[canonicalCommand(arg)]; reserved {
				return "", nil, fmt.Errorf("%q is an adg command; use -g %s to select that group", arg, arg)
			}
			if arg == "" || len(args) == 1 {
				return "", nil, errors.New("expected GROUP followed by adb arguments")
			}
			return arg, args[1:], nil
		}
		if arg == "-" || arg == "--" {
			return "", nil, errors.New("use -g GROUP to select a group explicitly")
		}
		name, _, inline := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		n := 1
		// Every registered connection option takes one string value.
		if !inline && flags.Lookup(name) != nil && len(args) > 1 {
			n++
		}
		if err := flags.Parse(args[:n]); err != nil {
			return "", nil, err
		}
		args = args[n:]
		if name == "g" || name == "group" {
			if o.group == "" {
				return "", nil, errors.New("group name cannot be empty")
			}
			return o.group, args, nil
		}
	}
	return "", nil, errors.New("a group is required; use adg GROUP <adb arguments>")
}

func runGroupADB(args []string) error {
	flags := flag.NewFlagSet("adg", flag.ContinueOnError)
	flags.Usage = func() { printUsage(flags.Output()) }
	o := bindClientFlags(flags)
	path, err := defaultTunnelConfig()
	if err != nil {
		return err
	}
	file := flags.String("config", path, "client configuration file")
	flags.StringVar(file, "f", path, "short for -config")
	group, adbArgs, err := parseGroupArgs(flags, o, args)
	if err != nil {
		return err
	}
	if err := validateADBRoute(adbArgs); err != nil {
		return err
	}
	adb, err := exec.LookPath("adb")
	if err != nil {
		return err
	}
	if _, err := o.loadConfig(*file, flags); err != nil {
		return err
	}
	s, err := o.ensureGroupTunnel(group)
	if err != nil {
		return err
	}
	host, port, err := net.SplitHostPort(s.Config.Listen)
	if err != nil {
		return err
	}
	// adb constructs its socket specification as tcp:HOST:PORT.
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	argv := append([]string{"adb", "-H", host, "-P", port}, adbArgs...)
	return syscall.Exec(adb, argv, os.Environ())
}
