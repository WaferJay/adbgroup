package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
)

type packManifest struct {
	Version int      `json:"version"`
	Name    string   `json:"name"`
	Servers []string `json:"servers"`
}

type credentials struct{ cert, key, ca []byte }

func (c credentials) packEntries() map[string][]byte {
	return map[string][]byte{"client.pem": c.cert, "client.key": c.key, "ca.pem": c.ca}
}

func certificates(data []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	for len(bytes.TrimSpace(data)) > 0 {
		data = bytes.TrimSpace(data)
		if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("expected PEM certificates")
		}
		b, rest := pem.Decode(data)
		if b == nil || b.Type != "CERTIFICATE" {
			return nil, errors.New("invalid PEM certificate")
		}
		c, err := x509.ParseCertificate(b.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, c)
		data = rest
	}
	if len(certs) == 0 {
		return nil, errors.New("empty certificate chain")
	}
	return certs, nil
}

type credentialInfo struct {
	leaf           *x509.Certificate
	caFingerprints string
}

func (c credentials) validate() (*credentialInfo, error) {
	chain, err := certificates(c.cert)
	if err != nil {
		return nil, err
	}
	if _, err = tls.X509KeyPair(c.cert, c.key); err != nil {
		return nil, err
	}
	roots, err := certificates(c.ca)
	if err != nil {
		return nil, fmt.Errorf("invalid server CA: %w", err)
	}
	leaf := chain[0]
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) || leaf.IsCA || len(leaf.URIs) != 1 {
		return nil, errors.New("expected a currently valid client certificate with one identity URI")
	}
	if _, err = pki.IdentityURI(leaf.URIs[0].String()); err != nil {
		return nil, err
	}
	if len(leaf.ExtKeyUsage) > 0 && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) && !slices.Contains(leaf.ExtKeyUsage, x509.ExtKeyUsageAny) {
		return nil, errors.New("certificate does not allow client authentication")
	}
	var fingerprints []string
	for _, root := range roots {
		fingerprints = append(fingerprints, fmt.Sprintf("%x", sha256.Sum256(root.Raw)))
	}
	slices.Sort(fingerprints)
	return &credentialInfo{leaf: leaf, caFingerprints: strings.Join(slices.Compact(fingerprints), ", ")}, nil
}

func origin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" && u.Path != "/" || strings.ContainsAny(u.Host, "*\\") {
		return nil, errors.New("server must be a concrete HTTPS origin")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid HTTPS port")
		}
	}
	u.Path = ""
	return u, nil
}

func packServers(cfg gateway.Config, server string) ([]string, error) {
	data, err := os.ReadFile(cfg.API.Certificate)
	if err != nil {
		return nil, err
	}
	chain, err := certificates(data)
	if err != nil {
		return nil, err
	}
	cert := chain[0]
	if server != "" {
		u, err := origin(server)
		if err != nil {
			return nil, err
		}
		if err = cert.VerifyHostname(u.Hostname()); err != nil {
			return nil, err
		}
		return []string{u.String()}, nil
	}
	_, port, err := net.SplitHostPort(cfg.API.Listen)
	if err != nil {
		return nil, err
	}
	names := cfg.PKI.ServerNames
	if len(names) == 0 {
		names = slices.Clone(cert.DNSNames)
		for _, ip := range cert.IPAddresses {
			names = append(names, ip.String())
		}
	}
	var servers []string
	for _, name := range names {
		name = strings.TrimSpace(name)
		addr := "https://" + net.JoinHostPort(name, port)
		if _, err := origin(addr); err == nil && cert.VerifyHostname(name) == nil && !slices.Contains(servers, addr) {
			servers = append(servers, addr)
		}
	}
	return servers, nil
}

const maxPackEntry = 1 << 20

func boundedRead(r io.Reader, limit int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if int64(len(b)) > limit {
		return nil, errors.New("credential file exceeds size limit")
	}
	return b, err
}
func readLimitedFile(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return boundedRead(f, limit)
}

func readCredentials(cert, key, ca string) (credentials, error) {
	var c credentials
	for _, item := range []struct {
		path string
		dst  *[]byte
	}{{cert, &c.cert}, {key, &c.key}, {ca, &c.ca}} {
		data, err := readLimitedFile(item.path, maxPackEntry)
		if err != nil {
			return c, err
		}
		*item.dst = data
	}
	return c, nil
}

func writeClientPack(cfg gateway.Config, dir, dest, server, serverCA string) error {
	servers, err := packServers(cfg, server)
	if err != nil {
		return err
	}
	c, err := readCredentials(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.pem"))
	if err != nil {
		return err
	}
	absoluteDir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	manifest := packManifest{Version: 1, Name: filepath.Base(absoluteDir), Servers: servers}
	entries := c.packEntries()
	if serverCA != "" {
		if _, err := certificates(c.ca); err != nil {
			return err
		}
		c.ca, err = readLimitedFile(serverCA, maxPackEntry)
		if err != nil {
			return err
		}
		entries["server-ca.pem"] = c.ca
	}
	if _, err = c.validate(); err != nil {
		return err
	}
	// Explicit exports must fail if the destination already exists.
	return writePackArchive(dest, manifest, entries, os.Link)
}

func writePackArchive(dest string, manifest packManifest, entries map[string][]byte, publish func(string, string) error) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	entries["manifest.json"] = data
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".adgpack-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	z := zip.NewWriter(tmp)
	for name, data := range entries {
		w, err := z.Create(name)
		if err != nil {
			return err
		}
		if _, err = w.Write(data); err != nil {
			return err
		}
	}
	if err = z.Close(); err != nil {
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	return publish(tmp.Name(), dest)
}

func readClientPack(path string) (packManifest, credentials, error) {
	var m packManifest
	var c credentials
	data, err := readLimitedFile(path, 6*maxPackEntry)
	if err != nil {
		return m, c, err
	}
	z, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return m, c, err
	}
	entries := map[string][]byte{}
	for _, f := range z.File {
		switch f.Name {
		case "manifest.json", "client.pem", "client.key", "ca.pem", "server-ca.pem":
		default:
			return m, c, fmt.Errorf("unexpected archive entry %q", f.Name)
		}
		if _, ok := entries[f.Name]; ok || !f.Mode().IsRegular() {
			return m, c, fmt.Errorf("invalid archive entry %q", f.Name)
		}
		r, err := f.Open()
		if err != nil {
			return m, c, err
		}
		b, err := boundedRead(r, maxPackEntry)
		r.Close()
		if err != nil {
			return m, c, err
		}
		entries[f.Name] = b
	}
	if err = json.Unmarshal(entries["manifest.json"], &m); err != nil {
		return m, c, err
	}
	if m.Version != 1 {
		return m, c, fmt.Errorf("unsupported pack version %d", m.Version)
	}
	for _, s := range m.Servers {
		if _, err := origin(s); err != nil {
			return m, c, err
		}
	}
	c = credentials{entries["client.pem"], entries["client.key"], entries["ca.pem"]}
	if ca, ok := entries["server-ca.pem"]; ok {
		if _, err := certificates(c.ca); err != nil {
			return m, c, err
		}
		c.ca = ca
	}
	return m, c, nil
}

func runCertPack(args []string) error {
	flags := newCommandFlags("cert pack")
	config := flags.String("config", "config.yaml", "server configuration")
	dir := flags.String("credentials", "", "existing client export directory")
	dest := flags.String("bundle", "", "new .adgpack file")
	server := flags.String("server", "", "preferred HTTPS origin")
	ca := flags.String("server-ca", "", "HTTPS server CA trust file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *dir == "" || *dest == "" {
		return errors.New("cert pack requires -credentials and -bundle")
	}
	cfg, err := load(*config)
	if err != nil {
		return err
	}
	return writeClientPack(cfg, *dir, *dest, *server, *ca)
}

func issueClientPack(cfg gateway.Config, caCert, caKey, identity, out string, days int, server, serverCA string) error {
	out = filepath.Clean(out)
	if _, err := os.Lstat(out); err == nil {
		return fmt.Errorf("output directory already exists: %s", out)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(out)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".issue-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	dir := filepath.Join(stage, filepath.Base(out))
	if _, err = pki.IssueClient(caCert, caKey, identity, dir, days); err != nil {
		return err
	}
	if err = writeClientPack(cfg, dir, filepath.Join(dir, "client.adgpack"), server, serverCA); err != nil {
		return err
	}
	return os.Rename(dir, out)
}
