package main

import (
	"strings"
	"testing"
)

func TestValidateADBRoute(t *testing.T) {
	for _, tc := range []struct {
		args     []string
		conflict string
	}{
		{[]string{"-H", "example.net", "devices"}, "-H"},
		{[]string{"-Hexample.net", "devices"}, "-H"},
		{[]string{"-d", "-P", "5037", "shell"}, "-P"},
		{[]string{"-t123", "-P5037", "shell"}, "-P"},
		{[]string{"-s123abc", "-L", "tcp:5037", "shell"}, "-L"},
		{[]string{"-e", "--exit-on-write-error", "-t", "8", "-L", "tcp:5037"}, "-L"},
		{[]string{"server", "nodaemon", "-a", "--one-device", "USB", "-P5037"}, "-P"},
		{[]string{"fork-server", "--reply-fd", "3", "-Hhost"}, "-H"},
		{[]string{"-s", "-P5037", "shell", "echo", "-H", "-P", "-L"}, ""},
		{[]string{"wait-for-device", "shell", "echo", "-Hhost"}, ""},
		{[]string{"shell", "--one-device", "-L", "hello"}, ""},
		{[]string{"devices", "-l"}, ""},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			err := validateADBRoute(tc.args)
			if tc.conflict == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.conflict) {
				t.Errorf("got %v, want conflict with %s", err, tc.conflict)
			}
		})
	}
}
