package main

import (
	"flag"
	"fmt"
	"io"
)

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage: adbgroup <command> [options]

Commands:
  check          Validate the server configuration
  serve          Start the ADB gateway server
  cert           Issue client credentials
  tunnel (t)     Start or manage local ADB tunnels
  groups (g)     List authorized groups
  devices (d)    List devices and lock status in a group
  daemon         Manage the background tunnel daemon

Options:
  -h, -help      Show help (also accepts --help)

Run "adbgroup <command> -help" for command usage and options.
`)
}

var commandHelp = map[string]struct {
	syntax      string
	description string
}{
	"check":   {"check [options]", "Validate the server configuration without starting the gateway."},
	"serve":   {"serve [options]", "Start the HTTPS API and ADB gateway server."},
	"cert":    {"cert [options] -user <name> -out <directory>", "Issue client credentials. Use -identity instead of -user for an explicit identity URI."},
	"groups":  {"groups [options]", "List authorized groups. Alias: g."},
	"devices": {"devices [options]", "List devices and lock status in the selected group. Alias: d."},
	"tunnel": {"tunnel [options]\n       adbgroup tunnel <list|stop> [options]", `Start a local ADB tunnel managed by the background daemon. Alias: t.
Use -no-daemon to run in the foreground.

Commands:
  list           List managed tunnels (supports -json)
  stop <id>      Stop and remove a managed tunnel`},
	"tunnel list": {"tunnel list [options]", "List tunnels managed by the current user's daemon."},
	"tunnel stop": {"tunnel stop <id>", "Stop and remove the managed tunnel with the given ID."},
	"daemon": {"daemon <start|status|stop> [options]", `Manage the current user's background tunnel daemon.

Commands:
  start          Start the daemon if it is not running
  status         Show whether the daemon is running
  stop           Stop all managed tunnels and the daemon`},
	"daemon start":  {"daemon start", "Start the background tunnel daemon if it is not running."},
	"daemon status": {"daemon status", "Show whether the background tunnel daemon is running."},
	"daemon stop":   {"daemon stop", "Stop all managed tunnels and wait for the background daemon to exit."},
	"daemon run":    {"daemon run", "Run the tunnel daemon in the foreground (internal command)."},
}

func newCommandFlags(command string) *flag.FlagSet {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.Usage = func() {
		help := commandHelp[command]
		fmt.Fprintf(flags.Output(), "Usage: adbgroup %s\n\n%s\n\nOptions:\n", help.syntax, help.description)
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "  -h, -help\n    \tshow help (also accepts --help)")
	}
	return flags
}
