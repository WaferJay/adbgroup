package main

import (
	"flag"
	"fmt"
	"io"
)

func printUsage(out io.Writer) {
	fmt.Fprint(out, `Usage: adg <command> [options]
       adg
       adg [connection options] GROUP [adb arguments...]
       adg [connection options] @GROUP [adb arguments...]

Find a group and run native adb:
  adg groups
  adg team-a devices -l
  adg team-a -s USB_SERIAL_A shell
  adg -f staging.yaml @devices devices -l

Select a group for this terminal (requires bash or zsh shell integration):
  adg                  Choose a group from a menu (same as "adg use")
  adg team-a           Same as "adg use team-a"
  adg @devices         Select the group named "devices"
  adg unuse            Restore the terminal's previous ADB socket

With shell integration, the first device query or tunnel creation without
an explicit group selects the configured group if this terminal has none.

Put connection options (-f/-config, -s/-server, -d/-credentials, -cert,
-key, -ca) before the group. Use @GROUP for a group named like an adg
command or alias. Use @@name for a group named @name.
Arguments after the group run adb once, independently of
the terminal selection. This requires adb on PATH and works without shell
initialization. The group supplies the ADB server endpoint, so adb server
options -H, -P, and -L conflict.

Commands:
  check          Validate the server configuration
  serve          Start the ADB gateway server
  cert           Issue client credentials
  tunnel (t)     Start or manage local ADB tunnels
  groups (g)     List authorized groups
  devices (d)    List devices and lock status in a group
  daemon         Manage the background tunnel daemon
  use            Select a group in the current terminal
  unuse          Restore the terminal's previous ADB socket
  shell-init     Print zsh or bash integration

Options:
  -h, -help      Show help (also accepts --help)

Run "adg <command> -help" for command usage and options.
`)
}

var commandHelp = map[string]struct {
	syntax      string
	description string
}{
	"check":   {"check [options]", "Validate the server configuration without starting the gateway."},
	"serve":   {"serve [options]", "Start the HTTPS API and ADB gateway server."},
	"cert":    {"cert [options] -user <name> -out <directory>", "Issue client credentials. Use -identity instead of -user for an explicit identity URI."},
	"groups":  {"groups [options]", "List authorized groups. A leading * marks the current group. Alias: g."},
	"devices": {"devices [GROUP] [options]", "List devices and lock status in a group. Alias: d.\nExample: adg devices @team-a --json. Without GROUP, an unselected terminal with shell integration adopts the configured group."},
	"use": {"use [GROUP] [options]", `Select a group in the current terminal. Both "adg" and "adg use" open the numbered menu.
You can also switch with "adg GROUP" or "adg @GROUP" for a reserved name.
Export ADBGROUP_GROUP with the selected group name.
Reuse a normal tunnel or create one on an OS-assigned loopback port.
Managed normal tunnels keep their port and reconnect after server/network outages.
Requires shell integration: eval "$(command adg shell-init zsh)" (or bash).
Client connection options override the current selection. No write lease is selected.`},
	"unuse": {"unuse", "Restore ADB_SERVER_SOCKET from before the first use and unset ADBGROUP_GROUP, keeping background tunnels running."},
	"shell-init": {"shell-init <zsh|bash>", `Print shell integration. Add eval "$(command adg shell-init zsh)" to ~/.zshrc,
or use bash and add it to the startup file loaded by your bash terminal.
Use "adg" for the group menu, "adg GROUP" to switch this terminal, and "adg unuse" to restore it.
"adg devices" and tunnel creation can also select the configured group on first use.`},
	"tunnel": {"tunnel [GROUP] [options]\n       adg tunnel <list|stop> [options]", `Start a local ADB tunnel managed by the background daemon. Alias: t.
Use @list or @stop to select a group named like a tunnel subcommand.
Use -no-daemon to run in the foreground.
After connecting, managed normal tunnels reconnect automatically on the same port.
While reconnecting, new ADB requests fail; streams and forward/reverse rules end.
Foreground and write-lease tunnels require explicit retry.

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
		fmt.Fprintf(flags.Output(), "Usage: adg %s\n\n%s\n\nOptions:\n", help.syntax, help.description)
		flags.PrintDefaults()
		fmt.Fprintln(flags.Output(), "  -h, -help\n    \tshow help (also accepts --help)")
	}
	return flags
}
