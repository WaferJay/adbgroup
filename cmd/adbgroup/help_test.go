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
		{"help", []string{"-help"}, []string{"Usage: adbgroup <command>", "tunnel (t)", "daemon"}, false},
		{"short help", []string{"-h"}, []string{"Usage: adbgroup <command>"}, false},
		{"long help", []string{"--help"}, []string{"Usage: adbgroup <command>"}, false},
		{"check", []string{"check", "-help"}, []string{"Usage: adbgroup check", "-config", "config.yaml"}, false},
		{"serve", []string{"serve", "-help"}, []string{"Usage: adbgroup serve", "-config"}, false},
		{"cert", []string{"cert", "-help"}, []string{"Usage: adbgroup cert", "-user", "-identity", "-out", "-days"}, false},
		{"tunnel", []string{"tunnel", "-help"}, []string{"Usage: adbgroup tunnel", "stop <id>", "-no-daemon", "-listen"}, false},
		{"tunnel alias", []string{"t", "-h"}, []string{"Usage: adbgroup tunnel", "-listen"}, false},
		{"missing config", []string{"tunnel", "-config", "missing.yaml", "-help"}, []string{"Usage: adbgroup tunnel"}, false},
		{"tunnel list", []string{"tunnel", "list", "-help"}, []string{"Usage: adbgroup tunnel list", "-json"}, false},
		{"tunnel stop", []string{"tunnel", "stop", "-help"}, []string{"Usage: adbgroup tunnel stop <id>"}, false},
		{"tunnel stop alias", []string{"t", "stop", "--help"}, []string{"Usage: adbgroup tunnel stop <id>"}, false},
		{"daemon", []string{"daemon", "-help"}, []string{"Usage: adbgroup daemon", "start", "status", "stop"}, false},
		{"daemon start", []string{"daemon", "start", "-help"}, []string{"Usage: adbgroup daemon start"}, false},
		{"daemon status", []string{"daemon", "status", "-help"}, []string{"Usage: adbgroup daemon status"}, false},
		{"daemon stop", []string{"daemon", "stop", "-help"}, []string{"Usage: adbgroup daemon stop"}, false},
		{"no command", nil, []string{"Usage: adbgroup <command>", "a command is required"}, true},
		{"unknown command", []string{"unknown"}, []string{"Usage: adbgroup <command>", `unknown command`}, true},
		{"unknown flag", []string{"check", "-unknown"}, []string{"Usage: adbgroup check", "flag provided but not defined"}, true},
		{"extra argument", []string{"check", "extra"}, []string{"Usage: adbgroup check", "unexpected positional arguments"}, true},
		{"missing tunnel ID", []string{"t", "stop"}, []string{"Usage: adbgroup tunnel stop <id>"}, true},
		{"extra list argument", []string{"t", "list", "extra"}, []string{"Usage: adbgroup tunnel list"}, true},
		{"missing daemon command", []string{"daemon"}, []string{"Usage: adbgroup daemon", "a daemon command is required"}, true},
		{"unknown daemon command", []string{"daemon", "unknown"}, []string{"Usage: adbgroup daemon", "unknown daemon command"}, true},
		{"extra daemon argument", []string{"daemon", "start", "extra"}, []string{"Usage: adbgroup daemon start"}, true},
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
