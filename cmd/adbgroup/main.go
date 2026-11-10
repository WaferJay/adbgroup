package main

import (
	"adbgroup/internal/gateway"
	"adbgroup/internal/pki"
	"adbgroup/internal/tunnel"
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
	if err := run(); err != nil {
		slog.Error("adbgroup stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return errors.New("usage: adbgroup <check|tunnel|cert|serve> -config config.yaml")
	}
	command := os.Args[1]
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	file := flags.String("config", "config.yaml", "configuration file")
	var user, identity, out, caKey, caCert *string
	var days *int
	var serverURL, group, lease, listen, clientCert, clientKey, serverCA *string
	if command == "tunnel" {
		serverURL = flags.String("server", "", "HTTPS server origin")
		group = flags.String("group", "", "group")
		lease = flags.String("lease", "", "active write lease ID")
		listen = flags.String("listen", "127.0.0.1:15037", "local ADB listener")
		clientCert = flags.String("cert", "", "client certificate")
		clientKey = flags.String("key", "", "client private key")
		serverCA = flags.String("ca", "", "server CA trust bundle")
	}
	if command == "cert" {
		user = flags.String("user", "", "user name (identity becomes spiffe://adbgroup/USER)")
		identity = flags.String("identity", "", "explicit identity URI, instead of -user")
		out = flags.String("out", "", "new directory for client.pem, client.key and ca.pem")
		days = flags.Int("days", 365, "client certificate validity in days")
		caKey = flags.String("ca-key", "", "external signing CA private key")
		caCert = flags.String("ca-cert", "", "external signing CA certificate/chain (defaults to api.client_ca)")
	}
	if err := flags.Parse(os.Args[2:]); err != nil {
		return err
	}
	if command != "check" && command != "serve" && command != "tunnel" && command != "cert" {
		return errors.New("expected check, tunnel, cert or serve")
	}
	if flags.NArg() != 0 {
		return errors.New("unexpected positional arguments")
	}
	if command == "tunnel" {
		if *group == "" {
			return errors.New("-group is required")
		}
		client, err := tunnel.NewClient(*serverURL, *clientCert, *clientKey, *serverCA)
		if err != nil {
			return err
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return client.Run(ctx, *group, *lease, *listen)
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
		files, err := pki.IssueClient(*caCert, *caKey, *identity, *out, *days)
		if err != nil {
			return err
		}
		fmt.Printf("identity: %s\ncertificate: %s\nprivate key: %s\nCA certificate: %s\n", *identity, files.Certificate, files.Key, files.CA)
		if len(cfg.Identities[*identity]) == 0 {
			fmt.Printf("Add %q to identities and reload the configuration to grant group access.\n", *identity)
		}
		return nil
	}
	if err := ensureTLS(cfg); err != nil {
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
	engine := gateway.NewEngine(cfg)
	backend := gateway.Backend{Address: cfg.Backend.Address}
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
		if c, e := net.DialTimeout("tcp", cfg.Backend.Address, time.Second); e == nil {
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
	slog.Info("adbgroup ready", "api", cfg.API.Listen, "groups", len(cfg.Groups))
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
				e = errors.New("backend, API, PKI and group names require a full service restart")
			}
			if e != nil {
				slog.Error("configuration reload rejected", "error", e)
			} else {
				engine.Reload(next)
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
	listen, err := adbListenSocket(c.Address)
	if err != nil {
		errs <- err
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, path, "-L", listen, "server", "nodaemon")
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

func adbListenSocket(address string) (string, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("invalid backend address: %w", err)
	}
	// ADB only accepts the special hostname "localhost" for a smartsocket
	// listener. The gateway still dials the numeric loopback address from the
	// configuration, avoiding hostname resolution for backend connections.
	return "tcp:localhost:" + port, nil
}
