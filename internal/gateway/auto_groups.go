package gateway

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
)

var groupNamePattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

type AutoGroupsConfig struct {
	All        SummaryGroupConfig `yaml:"all"`
	Configured SummaryGroupConfig `yaml:"configured"`
	Discovery  SummaryGroupConfig `yaml:"discovery"`
	Serial     SerialGroupConfig  `yaml:"serial"`
	Model      ModelGroupConfig   `yaml:"model"`
}

type SummaryGroupConfig struct {
	Enabled bool   `yaml:"enabled"`
	Name    string `yaml:"name"`
}

type SerialGroupConfig struct {
	Enabled       bool   `yaml:"enabled"`
	Source        string `yaml:"source"`
	InheritAccess bool   `yaml:"inherit_access"`
}

type ModelGroupConfig struct {
	Enabled bool   `yaml:"enabled"`
	Source  string `yaml:"source"`
	Prefix  string `yaml:"prefix"`
}

func (a *AutoGroupsConfig) defaults() {
	if a.All.Name == "" {
		a.All.Name = "all"
	}
	if a.Configured.Name == "" {
		a.Configured.Name = "configured"
	}
	if a.Discovery.Name == "" {
		a.Discovery.Name = "discovery"
	}
	if a.Model.Prefix == "" {
		a.Model.Prefix = "model-"
	}
}

func validGeneratedName(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "*;") &&
		!strings.ContainsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}

func (c Config) pendingAutoGroup(name string) bool {
	a := c.AutoGroups
	a.defaults()
	if !validGeneratedName(name) {
		return false
	}
	return a.Serial.Enabled || a.Model.Enabled && strings.HasPrefix(name, a.Model.Prefix) && len(name) > len(a.Model.Prefix)
}

func (c Config) validateAutoGroups(names map[string]bool) error {
	a := c.AutoGroups
	a.defaults()
	for _, g := range []SummaryGroupConfig{a.Configured, a.Discovery, a.All} {
		if !g.Enabled {
			continue
		}
		if !groupNamePattern.MatchString(g.Name) || names[g.Name] {
			return fmt.Errorf("invalid or duplicate auto group: %q", g.Name)
		}
		names[g.Name] = true
	}
	for _, rule := range []struct {
		name, source string
		enabled      bool
	}{{"serial", a.Serial.Source, a.Serial.Enabled}, {"model", a.Model.Source, a.Model.Enabled}} {
		if rule.enabled && (rule.source == "" || !names[rule.source]) {
			return fmt.Errorf("auto_groups.%s.source must reference a static or enabled summary group: %q", rule.name, rule.source)
		}
	}
	if a.Model.Enabled && !validGeneratedName(a.Model.Prefix+"x") {
		return fmt.Errorf("invalid auto_groups.model.prefix: %q", a.Model.Prefix)
	}
	_, conflicts := c.effectiveConfig(nil)
	if len(conflicts) != 0 {
		return fmt.Errorf("auto group name conflicts: %s", strings.Join(conflicts, ", "))
	}
	return nil
}

func sortedSerials(set map[string]bool) []string {
	serials := make([]string, 0, len(set))
	for serial := range set {
		serials = append(serials, serial)
	}
	slices.Sort(serials)
	return serials
}

// effectiveConfig derives groups from immutable source configuration and the
// last model observed for each remembered serial. No derived groups feed back
// into source configuration or discovery.
func (c Config) effectiveConfig(known map[string]string) (Config, []string) {
	c = cloneConfig(c)
	c.AutoGroups.defaults()
	c.serialGroups = make(map[string]string)
	a := c.AutoGroups
	configured, discovered, all := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, group := range c.Groups {
		for _, serial := range group.Devices {
			configured[serial], all[serial] = true, true
		}
	}
	if a.Discovery.Enabled {
		for serial := range known {
			all[serial] = true
			if !configured[serial] {
				discovered[serial] = true
			}
		}
	}
	for _, item := range []struct {
		config SummaryGroupConfig
		set    map[string]bool
	}{{a.Configured, configured}, {a.Discovery, discovered}, {a.All, all}} {
		if item.config.Enabled {
			c.Groups = append(c.Groups, Group{Name: item.config.Name, Devices: sortedSerials(item.set)})
		}
	}
	serialGroups, modelGroups := map[string]Group{}, map[string]Group{}
	if a.Serial.Enabled {
		if source, ok := c.Group(a.Serial.Source); ok {
			for _, serial := range source.Devices {
				serialGroups[serial] = Group{Name: serial, Devices: []string{serial}}
			}
		}
	}
	if a.Model.Enabled {
		if source, ok := c.Group(a.Model.Source); ok {
			members := map[string]map[string]bool{}
			for _, serial := range source.Devices {
				if model := known[serial]; model != "" {
					name := a.Model.Prefix + model
					if members[name] == nil {
						members[name] = map[string]bool{}
					}
					members[name][serial] = true
				}
			}
			for name, set := range members {
				modelGroups[name] = Group{Name: name, Devices: sortedSerials(set)}
			}
		}
	}
	counts := map[string]int{}
	for _, group := range c.Groups {
		counts[group.Name]++
	}
	for name := range serialGroups {
		counts[name]++
	}
	for name := range modelGroups {
		counts[name]++
	}
	conflicts := map[string]bool{}
	for i, groups := range []map[string]Group{serialGroups, modelGroups} {
		names := map[string]bool{}
		for name := range groups {
			names[name] = true
		}
		for _, name := range sortedSerials(names) {
			if counts[name] != 1 || !validGeneratedName(name) {
				conflicts[name] = true
				continue
			}
			c.Groups = append(c.Groups, groups[name])
			if i == 0 {
				c.serialGroups[name] = name
			}
		}
	}
	return c, sortedSerials(conflicts)
}

func cloneConfig(c Config) Config {
	c.PKI.ServerNames = slices.Clone(c.PKI.ServerNames)
	c.Groups = slices.Clone(c.Groups)
	for i := range c.Groups {
		c.Groups[i].Devices = slices.Clone(c.Groups[i].Devices)
	}
	identities := make(map[string][]string, len(c.Identities))
	for identity, groups := range c.Identities {
		identities[identity] = slices.Clone(groups)
	}
	c.Identities = identities
	return c
}
