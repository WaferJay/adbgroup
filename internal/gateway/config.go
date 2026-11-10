package gateway

import (
	"fmt"
	"net"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Backend         BackendConfig       `yaml:"backend"`
	API             APIConfig           `yaml:"api"`
	PKI             PKIConfig           `yaml:"pki"`
	ReadWaitSeconds int                 `yaml:"read_wait_seconds"`
	ReadQueueLimit  int                 `yaml:"read_queue_limit"`
	Groups          []Group             `yaml:"groups"`
	Identities      map[string][]string `yaml:"identities"`
}
type BackendConfig struct {
	Binary  string `yaml:"binary"`
	Address string `yaml:"address"`
	Manage  bool   `yaml:"manage"`
}
type BackendEndpoint struct {
	DialAddress string
	Port        string
	ListenAll   bool
}
type APIConfig struct {
	Listen      string `yaml:"listen"`
	Certificate string `yaml:"certificate"`
	Key         string `yaml:"key"`
	ClientCA    string `yaml:"client_ca"`
}
type PKIConfig struct {
	Directory   string   `yaml:"directory"`
	ServerNames []string `yaml:"server_names"`
	AutoTLS     bool     `yaml:"-"`
}
type Group struct {
	Name    string   `yaml:"name" json:"name"`
	Devices []string `yaml:"devices" json:"devices"`
}

func (c *Config) Defaults() {
	if c.PKI.Directory == "" {
		c.PKI.Directory = "run/pki"
	}
	if c.API.Certificate == "" && c.API.Key == "" && c.API.ClientCA == "" {
		c.PKI.AutoTLS = true
		c.API.Certificate = filepath.Join(c.PKI.Directory, "server.pem")
		c.API.Key = filepath.Join(c.PKI.Directory, "server.key")
		c.API.ClientCA = filepath.Join(c.PKI.Directory, "ca.pem")
	}
	if c.Backend.Binary == "" {
		c.Backend.Binary = "adb"
	}
	if c.Backend.Address == "" {
		c.Backend.Address = "127.0.0.1:5037"
	}
	if c.ReadWaitSeconds == 0 {
		c.ReadWaitSeconds = 30
	}
	if c.ReadQueueLimit == 0 {
		c.ReadQueueLimit = 128
	}
}
func ParseBackendAddress(address string) (BackendEndpoint, error) {
	if !strings.Contains(address, ":") {
		port, err := parseBackendPort(address)
		if err != nil {
			return BackendEndpoint{}, err
		}
		return BackendEndpoint{DialAddress: net.JoinHostPort("127.0.0.1", port), Port: port, ListenAll: true}, nil
	}
	host, rawPort, err := net.SplitHostPort(address)
	if err != nil {
		return BackendEndpoint{}, err
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return BackendEndpoint{}, fmt.Errorf("host must be a numeric loopback address")
	}
	port, err := parseBackendPort(rawPort)
	if err != nil {
		return BackendEndpoint{}, err
	}
	return BackendEndpoint{DialAddress: net.JoinHostPort(ip.String(), port), Port: port}, nil
}

func parseBackendPort(port string) (string, error) {
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("port must be an integer between 1 and 65535")
	}
	return strconv.Itoa(n), nil
}
func (c Config) Validate() error {
	backend, err := ParseBackendAddress(c.Backend.Address)
	if err != nil {
		return fmt.Errorf("backend.address must be a port or numeric loopback address: %w", err)
	}
	if c.ReadWaitSeconds < 1 || c.ReadWaitSeconds > 3600 || c.ReadQueueLimit < 1 {
		return fmt.Errorf("invalid reader limits")
	}
	if c.API.Certificate == "" || c.API.Key == "" || c.API.ClientCA == "" {
		return fmt.Errorf("api.certificate, api.key and api.client_ca must be set together, or all omitted")
	}
	if c.API.Listen == "" {
		return fmt.Errorf("API listener is required")
	}
	apiHost, apiPort, err := net.SplitHostPort(c.API.Listen)
	if err != nil {
		return err
	}
	apiIP := net.ParseIP(apiHost)
	apiAll := apiHost == "" || apiIP != nil && apiIP.IsUnspecified()
	if backend.Port == apiPort && (backend.ListenAll || apiAll || backend.DialAddress == c.API.Listen) {
		return fmt.Errorf("listener addresses overlap")
	}
	names := map[string]bool{}
	for _, g := range c.Groups {
		if !regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`).MatchString(g.Name) || names[g.Name] {
			return fmt.Errorf("invalid or duplicate group: %q", g.Name)
		}
		seen := map[string]bool{}
		for _, s := range g.Devices {
			if s == "" || strings.ContainsAny(s, " \t\r\n:;") || seen[s] {
				return fmt.Errorf("invalid or duplicate serial %q", s)
			}
			seen[s] = true
		}
		names[g.Name] = true
	}
	for identity, groups := range c.Identities {
		if identity == "" {
			return fmt.Errorf("empty identity")
		}
		for _, g := range groups {
			if !names[g] {
				return fmt.Errorf("identity %s references unknown group %s", identity, g)
			}
		}
	}
	return nil
}
func (c Config) Group(name string) (Group, bool) {
	for _, g := range c.Groups {
		if g.Name == name {
			return g, true
		}
	}
	return Group{}, false
}
func (c Config) Allows(identity, group string) bool {
	for _, g := range c.Identities[identity] {
		if g == group {
			return true
		}
	}
	return false
}
func (g Group) Contains(serial string) bool {
	for _, s := range g.Devices {
		if s == serial {
			return true
		}
	}
	return false
}
func (c Config) ReadWait() time.Duration { return time.Duration(c.ReadWaitSeconds) * time.Second }
func (c Config) ReloadCompatible(n Config) bool {
	// Keep the group set fixed until restart; membership and ordering may change.
	if len(c.Groups) != len(n.Groups) {
		return false
	}
	for _, group := range c.Groups {
		_, ok := n.Group(group.Name)
		if !ok {
			return false
		}
	}
	c.Groups = nil
	c.Identities = nil
	c.ReadQueueLimit = 0
	c.ReadWaitSeconds = 0
	n.Groups = nil
	n.Identities = nil
	n.ReadQueueLimit = 0
	n.ReadWaitSeconds = 0
	return reflect.DeepEqual(c, n)
}
