package main

import (
	"fmt"
	"strings"
)

// validateADBRoute follows adb's global-option prefix. Everything from the
// subcommand onward belongs to adb or the device (for example, shell echo -P).
func validateADBRoute(args []string) error {
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "-s", "-t", "--one-device", "--reply-fd":
			i++ // The next token is a value, even if it starts with a dash.
		case "-d", "-e", "-a", "--exit-on-write-error", "server", "nodaemon", "fork-server":
			continue
		default:
			if strings.HasPrefix(arg, "-H") || strings.HasPrefix(arg, "-P") || arg == "-L" {
				return fmt.Errorf("adb option %s conflicts with the device group's tunnel address", arg[:2])
			}
			// adb also accepts -sSERIAL and -tID when the value starts with a digit.
			if len(arg) > 2 && (arg[:2] == "-s" || arg[:2] == "-t") && arg[2] >= '0' && arg[2] <= '9' {
				continue
			}
			return nil
		}
	}
	return nil
}
