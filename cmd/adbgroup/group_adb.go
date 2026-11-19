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

// A group ends adg option parsing. Everything after it belongs to adb.
func parseGroupArgs(flags *flag.FlagSet, o *tunnelOptions, args []string) (string, []string, error) {
	for len(args) > 0 {
		arg := args[0]
		if !strings.HasPrefix(arg, "-") {
			if _, reserved := commandHelp[canonicalCommand(arg)]; reserved {
				return "", nil, fmt.Errorf("%q is an adg command; use @%s to select that group", arg, arg)
			}
			group, err := positionalGroupName(arg)
			if err != nil {
				return "", nil, err
			}
			return group, args[1:], nil
		}
		if arg == "-" || arg == "--" {
			return "", nil, errors.New("use @GROUP to select a group explicitly")
		}
		var err error
		args, err = parseGroupOption(flags, args)
		if err != nil {
			return "", nil, err
		}
		if flagWasSet(flags, "group", "g") {
			if o.group == "" {
				return "", nil, errors.New("group name cannot be empty")
			}
			return o.group, args, nil
		}
	}
	return "", nil, errors.New("a group is required; use adg GROUP or adg @GROUP")
}

func runGroupCommand(args []string) error {
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
	if len(adbArgs) == 0 {
		if err := requireShell(); err != nil {
			return fmt.Errorf("%w; for a one-off adb command, append 'devices -l' to this invocation", err)
		}
		return o.selectTerminalGroup(group, *file, flags, os.Stdout)
	}
	if err := validateADBRoute(adbArgs); err != nil {
		return err
	}
	adb, err := exec.LookPath("adb")
	if err != nil {
		return err
	}
	if err := o.loadConfig(*file, flags); err != nil {
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
