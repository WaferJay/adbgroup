package gateway

import (
	"strings"
	"testing"
)

func TestConfiguredIdentityValidation(t *testing.T) {
	for _, tc := range []struct {
		identity string
		managed  bool
		wantErr  string
	}{
		{"urn:example:alice", true, "unsupported URN identity"},
		{"urn:example:alice", false, "unsupported URN identity"},
		{"UrN://example/alice", true, "unsupported URN identity"},
		{"mailto:alice@example.org", true, "absolute URI with a host"},
		{"mailto:alice@example.org", false, ""},
		{"spiffe://adbgroup/alice", true, ""},
		{"https://issuer.example/alice", true, ""},
	} {
		cfg := testConfig()
		cfg.PKI.AutoTLS = tc.managed
		cfg.Identities = map[string][]string{tc.identity: {"*"}}
		err := cfg.Validate()
		if tc.wantErr == "" {
			if err != nil {
				t.Errorf("identity %q (managed %t): %v", tc.identity, tc.managed, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), `identities["`+tc.identity+`"]`) || !strings.Contains(err.Error(), tc.wantErr) {
			t.Errorf("identity %q (managed %t): expected a config error containing %q, got %v", tc.identity, tc.managed, tc.wantErr, err)
		}
	}
}

func TestReloadRequiresRestartForPortAndGroupChanges(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"backend port", func(c *Config) { c.Backend.Address = "127.0.0.1:5038" }},
		{"API port", func(c *Config) { c.API.Listen = "127.0.0.1:8444" }},
		{"add group", func(c *Config) {
			c.Groups = append(c.Groups, Group{Name: "c"})
		}},
		{"remove group", func(c *Config) {
			c.Groups = c.Groups[:1]
			delete(c.Identities, "spiffe://test/bob")
		}},
		{"rename group", func(c *Config) {
			c.Groups[0].Name = "renamed"
			c.Identities["spiffe://test/alice"] = []string{"renamed"}
		}},
		{"port and membership together", func(c *Config) {
			c.API.Listen = "127.0.0.1:18080"
			c.Groups[0].Devices = []string{"B"}
			delete(c.Identities, "spiffe://test/alice")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, next := testConfig(), testConfig()
			tc.change(&next)
			if err := next.Validate(); err != nil {
				t.Fatalf("invalid test configuration: %v", err)
			}
			if old.ReloadCompatible(next) {
				t.Fatal("port or group change accepted for hot reload")
			}
		})
	}
}

func TestReloadAllowsMembershipPermissionsAndReaderLimits(t *testing.T) {
	cases := []struct {
		name   string
		change func(*Config)
	}{
		{"unchanged", func(*Config) {}},
		{"reorder groups", func(c *Config) { c.Groups[0], c.Groups[1] = c.Groups[1], c.Groups[0] }},
		{"remove member", func(c *Config) { c.Groups[0].Devices = []string{"B"} }},
		{"add shared member", func(c *Config) { c.Groups[0].Devices = append(c.Groups[0].Devices, "C") }},
		{"revoke identity", func(c *Config) { delete(c.Identities, "spiffe://test/alice") }},
		{"grant identity", func(c *Config) { c.Identities["spiffe://test/charlie"] = []string{"a", "b"} }},
		{"reader limits", func(c *Config) { c.ReadWaitSeconds, c.ReadQueueLimit = 60, 256 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			old, next := testConfig(), testConfig()
			tc.change(&next)
			if err := next.Validate(); err != nil {
				t.Fatalf("invalid test configuration: %v", err)
			}
			if !old.ReloadCompatible(next) {
				t.Fatal("membership, authorization or reader limit change requires restart")
			}
		})
	}
}
