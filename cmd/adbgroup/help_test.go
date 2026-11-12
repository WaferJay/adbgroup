package main

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestCLIHelpAndUsage(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, work := t.TempDir(), t.TempDir()
	for _, tc := range []struct {
		name string
		args []string
		want []string
		fail bool
	}{
		{"help", []string{"-help"}, []string{"Usage: adg <command>", "tunnel (t)", "groups (g)", "devices (d)", "daemon"}, false},
		{"short help", []string{"-h"}, []string{"Usage: adg <command>"}, false},
		{"long help", []string{"--help"}, []string{"Usage: adg <command>"}, false},
		{"check", []string{"check", "-help"}, []string{"Usage: adg check", "-config", "config.yaml"}, false},
		{"serve", []string{"serve", "-help"}, []string{"Usage: adg serve", "-config"}, false},
		{"cert", []string{"cert", "-help"}, []string{"Usage: adg cert", "-user", "-identity", "-out", "-days"}, false},
		{"tunnel", []string{"tunnel", "-help"}, []string{"Usage: adg tunnel", "stop <id>", "-no-daemon", "-listen"}, false},
		{"tunnel alias", []string{"t", "-h"}, []string{"Usage: adg tunnel", "-listen"}, false},
		{"groups", []string{"groups", "-help"}, []string{"Usage: adg groups", "-json", "-server"}, false},
		{"groups alias", []string{"g", "--help"}, []string{"Usage: adg groups", "-json"}, false},
		{"devices", []string{"devices", "-help"}, []string{"Usage: adg devices", "-group", "-json"}, false},
		{"devices alias", []string{"d", "-help"}, []string{"Usage: adg devices", "-group"}, false},
		{"use", []string{"use", "--help"}, []string{"Usage: adg use", "shell-init", "-server"}, false},
		{"use group help", []string{"use", "team-a", "--help"}, []string{"Usage: adg use"}, false},
		{"unuse", []string{"unuse", "--help"}, []string{"Usage: adg unuse"}, false},
		{"shell init", []string{"shell-init", "--help"}, []string{"Usage: adg shell-init"}, false},
		{"unsupported shell", []string{"shell-init", "fish"}, []string{"zsh|bash"}, true},
		{"use without integration", []string{"use", "team-a"}, []string{"initialize shell integration"}, true},
		{"unuse without integration", []string{"unuse"}, []string{"initialize shell integration"}, true},
		{"missing config", []string{"groups", "-config", "missing.yaml", "-help"}, []string{"Usage: adg groups"}, false},
		{"tunnel list", []string{"tunnel", "list", "-help"}, []string{"Usage: adg tunnel list", "-json"}, false},
		{"tunnel stop", []string{"tunnel", "stop", "-help"}, []string{"Usage: adg tunnel stop <id>"}, false},
		{"tunnel stop alias", []string{"t", "stop", "--help"}, []string{"Usage: adg tunnel stop <id>"}, false},
		{"daemon", []string{"daemon", "-help"}, []string{"Usage: adg daemon", "start", "status", "stop"}, false},
		{"daemon start", []string{"daemon", "start", "-help"}, []string{"Usage: adg daemon start"}, false},
		{"daemon status", []string{"daemon", "status", "-help"}, []string{"Usage: adg daemon status"}, false},
		{"daemon stop", []string{"daemon", "stop", "-help"}, []string{"Usage: adg daemon stop"}, false},
		{"no command", nil, []string{"Usage: adg <command>", "a command is required"}, true},
		{"unknown command", []string{"unknown"}, []string{"Usage: adg <command>", `unknown command`}, true},
		{"unknown flag", []string{"check", "-unknown"}, []string{"Usage: adg check", "flag provided but not defined"}, true},
		{"extra argument", []string{"devices", "extra"}, []string{"Usage: adg devices", "unexpected positional arguments"}, true},
		{"missing tunnel ID", []string{"t", "stop"}, []string{"Usage: adg tunnel stop <id>"}, true},
		{"extra list argument", []string{"t", "list", "extra"}, []string{"Usage: adg tunnel list"}, true},
		{"missing daemon command", []string{"daemon"}, []string{"Usage: adg daemon", "a daemon command is required"}, true},
		{"unknown daemon command", []string{"daemon", "unknown"}, []string{"Usage: adg daemon", "unknown daemon command"}, true},
		{"extra daemon argument", []string{"daemon", "start", "extra"}, []string{"Usage: adg daemon start"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, tc.args...)
			cmd.Env = append(os.Environ(), "ADBGROUP_TEST_CLI=1", "HOME="+home)
			cmd.Dir = work
			output, err := cmd.CombinedOutput()
			if (err != nil) != tc.fail {
				t.Fatalf("exit error = %v, want failure %v:\n%s", err, tc.fail, output)
			}
			for _, want := range tc.want {
				if !strings.Contains(string(output), want) {
					t.Errorf("output missing %q:\n%s", want, output)
				}
			}
		})
	}
	for _, dir := range []string{home, work} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("help or invalid usage created files in %s: %v", dir, entries)
		}
	}
}
