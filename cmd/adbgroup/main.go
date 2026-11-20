package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
	"github.com/WaferJay/adbgroup/internal/tunnel"
	"gopkg.in/yaml.v3"
)

func load(path string) (gateway.Config, error) {
	var c gateway.Config
	b, err := os.Open(path)
	if err != nil {
		return c, err
	}
	defer b.Close()
	d := yaml.NewDecoder(b)
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		return c, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, errors.New("exactly one YAML document required")
	}
	c.Defaults()
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, err
	}
	for _, p := range []*string{&c.API.Certificate, &c.API.Key, &c.API.ClientCA, &c.PKI.Directory} {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(base, *p)
		}
	}
	if strings.Contains(c.Backend.Binary, "/") && !filepath.IsAbs(c.Backend.Binary) {
		c.Backend.Binary = filepath.Join(base, c.Backend.Binary)
	}
	return c, c.Validate()
}
func main() {
	if err := run(); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("adg stopped", "error", err)
		os.Exit(1)
	}
}

const defaultTunnelServer = "https://127.0.0.1:8443"
const tunnelConfigPath = ".config/adbgroup/tunnel.yaml"

type tunnelOptions struct {
	server, group, lease, listen string
	credentials, cert, key, ca   string
	profile                      string
}

type tunnelFile struct {
	Server      string `yaml:"server"`
	Listen      string `yaml:"listen"`
	Credentials string `yaml:"credentials"`
	Certificate string `yaml:"cert"`
	Key         string `yaml:"key"`
	CA          string `yaml:"ca"`
}

func defaultTunnelConfig() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory: %w", err)
	}
	return filepath.Join(home, tunnelConfigPath), nil
}

func loadTunnelFile(path string) (tunnelFile, bool, error) {
	var c tunnelFile
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, false, nil
	}
	if err != nil {
		return c, false, err
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if err = d.Decode(&c); err != nil {
		if strings.Contains(err.Error(), "field group not found in type ") {
			return c, true, fmt.Errorf("remove the top-level group: field and specify the group on the command line: %w", err)
		}
		return c, true, err
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return c, true, errors.New("exactly one YAML document required")
	}
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return c, true, err
	}
	if c.Credentials == "" {
		c.Credentials = base
	}
	for _, value := range []*string{&c.Credentials, &c.Certificate, &c.Key, &c.CA} {
		if *value != "" && !filepath.IsAbs(*value) {
			*value = filepath.Join(base, *value)
		}
	}
	return c, true, nil
}

func flagWasSet(flags *flag.FlagSet, names ...string) bool {
	set := false
	flags.Visit(func(f *flag.Flag) {
		for _, name := range names {
			if f.Name == name {
				set = true
			}
		}
	})
	return set
}

func (o *tunnelOptions) applyFile(c tunnelFile, flags *flag.FlagSet) {
	credentialsSet := flagWasSet(flags, "credentials", "d")
	if c.Server != "" && !flagWasSet(flags, "server", "s") {
		o.server = c.Server
	}
	if c.Listen != "" && !flagWasSet(flags, "listen", "l") {
		o.listen = c.Listen
	}
	if c.Credentials != "" && !credentialsSet {
		o.credentials = c.Credentials
	}
	if c.Certificate != "" && !credentialsSet && !flagWasSet(flags, "cert") {
		o.cert = c.Certificate
	}
	if c.Key != "" && !credentialsSet && !flagWasSet(flags, "key") {
		o.key = c.Key
	}
	if c.CA != "" && !credentialsSet && !flagWasSet(flags, "ca") {
		o.ca = c.CA
	}
}

func (o *tunnelOptions) loadConfig(path string, flags *flag.FlagSet) error {
	if flagWasSet(flags, "config", "f") {
		if flagWasSet(flags, "p", "profile") {
			return errors.New("-profile and -config cannot be combined")
		}
	} else {
		if flagWasSet(flags, "p", "profile") && o.profile == "" {
			return errors.New("empty profile name")
		}
		name, c, err := lookupProfile(filepath.Join(filepath.Dir(path), "profiles"), o.profile)
		if err != nil {
			return err
		}
		if name != "" {
			o.profile = name
			o.applyFile(c, flags)
			return nil
		}
	}
	c, found, err := loadTunnelFile(path)
	if err != nil {
		return fmt.Errorf("load client config %s: %w", path, err)
	}
	if !found && flagWasSet(flags, "config", "f") {
		return fmt.Errorf("client config %s does not exist", path)
	}
	o.applyFile(c, flags)
	return nil
}

func bindClientFlags(flags *flag.FlagSet) *tunnelOptions {
	o := &tunnelOptions{server: defaultTunnelServer, listen: "127.0.0.1:15037", credentials: "."}
	flags.StringVar(&o.profile, "profile", "", "client profile name")
	flags.StringVar(&o.profile, "p", "", "short for -profile")
	flags.StringVar(&o.server, "server", o.server, "HTTPS server origin")
	flags.StringVar(&o.server, "s", o.server, "short for -server")
	flags.StringVar(&o.group, "group", "", "legacy explicit group selector")
	flags.StringVar(&o.group, "g", "", "legacy short form of -group")
	flags.StringVar(&o.credentials, "credentials", o.credentials, "directory containing client.pem, client.key and ca.pem")
	flags.StringVar(&o.credentials, "d", o.credentials, "short for -credentials")
	flags.StringVar(&o.cert, "cert", "", "client certificate (defaults to CREDENTIALS/client.pem)")
	flags.StringVar(&o.key, "key", "", "client private key (defaults to CREDENTIALS/client.key)")
	flags.StringVar(&o.ca, "ca", "", "server CA trust bundle (defaults to CREDENTIALS/ca.pem)")
	return o
}

func bindTunnelFlags(flags *flag.FlagSet) *tunnelOptions {
	o := bindClientFlags(flags)
	flags.StringVar(&o.lease, "lease", "", "active write lease ID")
	flags.StringVar(&o.lease, "w", "", "short for -lease")
	flags.StringVar(&o.listen, "listen", o.listen, "local ADB listener: port or numeric loopback address")
	flags.StringVar(&o.listen, "l", o.listen, "short for -listen")
	return o
}

func (o *tunnelOptions) credentialFiles() (string, string, string) {
	cert, key, ca := o.cert, o.key, o.ca
	if cert == "" {
		cert = filepath.Join(o.credentials, "client.pem")
	}
	if key == "" {
		key = filepath.Join(o.credentials, "client.key")
	}
	if ca == "" {
		ca = filepath.Join(o.credentials, "ca.pem")
	}
	return cert, key, ca
}

func canonicalCommand(command string) string {
	switch command {
	case "t":
		return "tunnel"
	case "g":
		return "groups"
	case "d":
		return "devices"
	}
	return command
}

func runClientCommand(command string, args []string) error {
	if command == "tunnel" && len(args) > 0 && (args[0] == "list" || args[0] == "stop") {
		return runTunnelCommand(args, os.Stdout)
	}
	flags := newCommandFlags(command)
	path, err := defaultTunnelConfig()
	if err != nil {
		return err
	}
	file := flags.String("config", path, "configuration file")
	flags.StringVar(file, "f", path, "short for -config")
	var o *tunnelOptions
	var noDaemon, jsonOutput bool
	if command == "tunnel" {
		o = bindTunnelFlags(flags)
		flags.BoolVar(&noDaemon, "no-daemon", false, "run the tunnel in the foreground")
	} else {
		o = bindClientFlags(flags)
		flags.BoolVar(&jsonOutput, "json", false, "print JSON instead of a table")
	}
	if err := parseClientGroupArgs(flags, args); err != nil {
		return err
	}
	if err := o.loadConfig(*file, flags); err != nil {
		return err
	}
	var currentGroup string
	if command != "tunnel" {
		if active := applySelection(o, flags); active != nil {
			currentGroup = active.Config.Group
		}
		if command == "devices" && o.group == "" {
			if connectionFlagsSet(flags) {
				return errors.New("group required with connection options: run adg devices GROUP")
			}
			return errors.New("group required: run adg devices GROUP or select a group with adg use GROUP")
		}
	} else {
		if o.group == "" {
			return errors.New("group required: run adg tunnel GROUP")
		}
		if !noDaemon {
			return startManagedTunnel(o, os.Stdout)
		}
	}
	cert, key, ca := o.credentialFiles()
	client, err := tunnel.NewClient(o.server, cert, key, ca)
	if err != nil {
		return err
	}
	defer client.CloseIdleConnections()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	if command == "tunnel" {
		return client.Run(ctx, o.group, o.lease, o.listen)
	}
	return runQuery(ctx, client, command, o.group, currentGroup, jsonOutput, os.Stdout)
}

func run() error {
	if len(os.Args) < 2 {
		return runUse(nil, os.Stdout)
	}
	command := canonicalCommand(os.Args[1])
	switch command {
	case "-h", "-help", "--help":
		printUsage(os.Stdout)
		return flag.ErrHelp
	case "profile":
		return runProfile(os.Args[2:], os.Stdout)
	case "cert":
		if len(os.Args) > 2 && os.Args[2] == "pack" {
			return runCertPack(os.Args[3:])
		}
	case "check", "serve":
	case "devices", "groups", "tunnel":
		return runClientCommand(command, os.Args[2:])
	case "--shell-client": // Shell functions loaded before upgrading forward queries and tunnels here.
		if len(os.Args) < 3 {
			return errors.New("devices or tunnel command required")
		}
		clientCommand := canonicalCommand(os.Args[2])
		if clientCommand != "devices" && clientCommand != "tunnel" {
			return errors.New("devices or tunnel command required")
		}
		return runClientCommand(clientCommand, os.Args[3:])
	case "daemon":
		return runDaemonCommand(os.Args[2:], os.Stdout)
	case "shell-init":
		return runShellInit(os.Args[2:], os.Stdout)
	case "use":
		return runUse(os.Args[2:], os.Stdout)
	case "unuse":
		return runUnuse(os.Args[2:])
	default:
		return runGroupCommand(os.Args[1:])
	}
	flags := newCommandFlags(command)
	file := flags.String("config", "config.yaml", "configuration file")
	var user, identity, out, caKey, caCert, serverOrigin, serverCA *string
	var days *int
	if command == "cert" {
		serverOrigin = flags.String("server", "", "preferred HTTPS origin in the pack")
		serverCA = flags.String("server-ca", "", "HTTPS server CA trust file for the pack")
		user = flags.String("user", "", "user name (identity becomes spiffe://adbgroup/USER)")
		identity = flags.String("identity", "", "explicit identity URI, instead of -user")
		out = flags.String("out", "", "new directory for client.pem, client.key, ca.pem and client.adgpack")
		days = flags.Int("days", 365, "client certificate validity in days")
		caKey = flags.String("ca-key", "", "external signing CA private key")
		caCert = flags.String("ca-cert", "", "external signing CA certificate/chain (defaults to api.client_ca)")
	}
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		flags.Usage()
		return errors.New("unexpected positional arguments")
	}
	cfg, err := load(*file)
	if err != nil {
		return err
	}
	if command == "check" {
		fmt.Println("configuration valid")
		return nil
	}
	if command == "cert" {
		if (*user == "") == (*identity == "") {
			return errors.New("provide exactly one of -user or -identity")
		}
		if *user != "" {
			*identity, err = pki.UserIdentity(*user)
			if err != nil {
				return err
			}
		}
		if _, err := pki.IdentityURI(*identity); err != nil {
			return err
		}
		if *out == "" || *days < 1 || *days > 3650 {
			return errors.New("-out is required; -days must be between 1 and 3650")
		}
		if !cfg.PKI.AutoTLS && *caKey == "" {
			return errors.New("external PKI requires -ca-key (and -ca-cert if the signer is not the first api.client_ca certificate)")
		}
		if err := ensureTLS(cfg); err != nil {
			return err
		}
		if *caKey == "" {
			*caKey = pki.ManagedFiles(cfg.PKI.Directory).CAKey
		}
		if *caCert == "" {
			*caCert = cfg.API.ClientCA
		}
		if err := pki.VerifyIssuerTrust(*caCert, cfg.API.ClientCA); err != nil {
			return err
		}
		err = issueClientPack(cfg, *caCert, *caKey, *identity, *out, *days, *serverOrigin, *serverCA)
		if err != nil {
			return err
		}
		fmt.Printf("identity: %s\ncertificate: %s\nprivate key: %s\nCA certificate: %s\n", *identity, filepath.Join(*out, "client.pem"), filepath.Join(*out, "client.key"), filepath.Join(*out, "ca.pem"))
		fmt.Println("credential pack:", filepath.Join(*out, "client.adgpack"))
		if len(cfg.Identities[*identity]) == 0 {
			fmt.Printf("Add %q to identities and reload the configuration to grant group access.\n", *identity)
		}
		return nil
	}
	if err := ensureTLS(cfg); err != nil {
		return err
	}
	if err := ensureStartupPacks(cfg); err != nil {
		return err
	}
	cert, err := tls.LoadX509KeyPair(cfg.API.Certificate, cfg.API.Key)
	if err != nil {
		return err
	}
	ca, err := os.ReadFile(cfg.API.ClientCA)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("no client CA certificates")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	backendEndpoint, err := gateway.ParseBackendAddress(cfg.Backend.Address)
	if err != nil {
		return err
	}
	engine := gateway.NewEngine(cfg)
	backend := gateway.Backend{Address: backendEndpoint.DialAddress}
	g := gateway.NewGateway(engine, backend)
	ts := tunnel.NewServer(ctx, g)
	defer ts.Close()
	apiListener, err := net.Listen("tcp", cfg.API.Listen)
	if err != nil {
		return err
	}
	defer apiListener.Close()
	server := &http.Server{Handler: (gateway.API{Engine: engine, Tunnels: ts.Handler()}).Handler(), ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}}
	errs := make(chan error, 3)
	go func() { errs <- server.ServeTLS(apiListener, "", "") }()

	if cfg.Backend.Manage {
		if c, e := net.DialTimeout("tcp", backendEndpoint.DialAddress, time.Second); e == nil {
			c.Close()
			return errors.New("managed backend port is already in use; choose a dedicated port or set manage: false")
		}
		go supervise(ctx, cfg.Backend, errs)
	}
	go engine.Run(ctx)
	go backend.Monitor(ctx, engine)
	go g.Forwards.Sweep(ctx)
	reload := make(chan os.Signal, 1)
	signal.Notify(reload, syscall.SIGHUP)
	defer signal.Stop(reload)
	slog.Info("adg ready", "api", cfg.API.Listen, "groups", len(engine.Config().Groups))
	for {
		select {
		case <-ctx.Done():
			shutdownCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			server.Shutdown(shutdownCtx)
			stop()
			ts.Close()
			g.Forwards.Close()
			return nil
		case err := <-errs:
			cancel()
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				return err
			}
			return nil
		case <-reload:
			next, e := load(*file)
			if e == nil && !cfg.ReloadCompatible(next) {
				e = errors.New("backend, API, PKI, group names and auto_groups require a full service restart")
			}
			if e == nil {
				e = engine.Reload(next)
			}
			if e != nil {
				slog.Error("configuration reload rejected", "error", e)
			} else {
				cfg = next
				slog.Info("configuration reloaded")
			}
		}
	}
}

func ensureTLS(c gateway.Config) error {
	if !c.PKI.AutoTLS {
		return nil
	}
	names, err := pki.ServerNames(c.PKI.ServerNames, c.API.Listen)
	if err != nil {
		return err
	}
	_, err = pki.Ensure(c.PKI.Directory, names)
	return err
}

func supervise(ctx context.Context, c gateway.BackendConfig, errs chan<- error) {
	path, err := exec.LookPath(c.Binary)
	if err != nil {
		errs <- err
		return
	}
	args, err := adbServerArgs(c.Address)
	if err != nil {
		errs <- err
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, path, args...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		cmd.Env = append(os.Environ(), "ADB_MDNS_AUTO_CONNECT=none", "ADB_LOCAL_TRANSPORT_MAX_PORT=0")
		err = cmd.Run()
		if ctx.Err() != nil {
			return
		}
		slog.Error("backend exited; restarting (all device sessions interrupted)", "error", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

func adbServerArgs(address string) ([]string, error) {
	endpoint, err := gateway.ParseBackendAddress(address)
	if err != nil {
		return nil, fmt.Errorf("invalid backend address: %w", err)
	}
	if endpoint.ListenAll {
		return []string{"-a", "-L", "tcp:" + endpoint.Port, "server", "nodaemon"}, nil
	}
	return []string{"-L", "tcp:localhost:" + endpoint.Port, "server", "nodaemon"}, nil
}
