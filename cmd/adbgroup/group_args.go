package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
)

// A single @ is syntax. A group whose name starts with @ can be written @@name.
func positionalGroupName(token string) (string, error) {
	name := strings.TrimPrefix(token, "@")
	if name == "" {
		return "", errors.New("group name cannot be empty")
	}
	return name, nil
}

// Consume an option and its value together so values are never interpreted as groups.
func parseGroupOption(flags *flag.FlagSet, args []string) ([]string, error) {
	n := 1
	name, _, inline := strings.Cut(strings.TrimLeft(args[0], "-"), "=")
	if f := flags.Lookup(name); f != nil && !inline && len(args) > 1 {
		if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !ok || !b.IsBoolFlag() {
			n++
		}
	}
	return args[n:], flags.Parse(args[:n])
}

// Client commands that take a group accept one positional selector among their options.
// Setting the registered flag gives it the same precedence as an explicit -group.
func parseClientGroupArgs(flags *flag.FlagSet, args []string) error {
	options := true
	var positional string
	for len(args) > 0 {
		token := args[0]
		switch {
		case options && token == "--":
			options = false
			args = args[1:]
		case options && len(token) > 1 && token[0] == '-':
			var err error
			args, err = parseGroupOption(flags, args)
			if err != nil {
				return err
			}
		default:
			if flags.Name() == "groups" {
				flags.Usage()
				return errors.New("unexpected positional arguments")
			}
			if flags.Name() == "tunnel" && (token == "list" || token == "stop") {
				return fmt.Errorf("use @%s to select the group named %s", token, token)
			}
			group, err := positionalGroupName(token)
			if err != nil {
				return err
			}
			if positional != "" {
				return errors.New("group specified more than once")
			}
			positional = group
			args = args[1:]
		}
	}
	if positional == "" {
		return nil
	}
	if flagWasSet(flags, "g", "group") {
		return errors.New("group specified more than once")
	}
	return flags.Set("group", positional)
}
