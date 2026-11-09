// Package pki implements local, administrator-controlled certificate provisioning.
// It has no network signing endpoint and never replaces an existing CA or key.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

type Files struct {
	CA, CAKey, Certificate, Key string
}

func ManagedFiles(dir string) Files {
	return Files{filepath.Join(dir, "ca.pem"), filepath.Join(dir, "ca.key"), filepath.Join(dir, "server.pem"), filepath.Join(dir, "server.key")}
}

// ServerNames includes local addresses without using DNS or trusting HTTP headers.
// Explicit names add hostnames/IPs used through NAT or a reverse proxy.
func ServerNames(explicit []string, listen string) ([]string, error) {
	names := map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true}
	add := func(name string) error {
		name = strings.TrimSpace(name)
		if ip := net.ParseIP(name); ip != nil {
			if ip.IsUnspecified() || ip.IsMulticast() {
				return fmt.Errorf("invalid server IP SAN %q", name)
			}
			names[ip.String()] = true
			return nil
		}
		name = strings.ToLower(strings.TrimSuffix(name, "."))
		if name == "" || len(name) > 253 {
			return fmt.Errorf("invalid server DNS SAN %q", name)
		}
		for _, label := range strings.Split(name, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return fmt.Errorf("invalid server DNS SAN %q", name)
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
					return fmt.Errorf("invalid server DNS SAN %q", name)
				}
			}
		}
		names[name] = true
		return nil
	}
	if hostname, err := os.Hostname(); err == nil {
		_ = add(hostname)
	}
	if host, _, err := net.SplitHostPort(listen); err == nil && host != "" && host != "0.0.0.0" && host != "::" {
		if err := add(host); err != nil {
			return nil, err
		}
	}
	if addresses, err := net.InterfaceAddrs(); err == nil {
		for _, address := range addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalUnicast() {
				_ = add(ip.String())
			}
		}
	}
	for _, name := range explicit {
		if err := add(name); err != nil {
			return nil, err
		}
	}
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out, nil
}

func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("PKI directory %s must be a real directory with mode 0700", dir)
	}
	return nil
}

func lock(dir string) (func(), error) {
	if err := privateDir(dir); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".init.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}

func exists(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("refusing non-regular PKI file %s", path)
	}
	return true, nil
}

func pairState(cert, key string) (bool, error) {
	c, err := exists(cert)
	if err != nil {
		return false, err
	}
	k, err := exists(key)
	if err != nil {
		return false, err
	}
	if c != k {
		return false, fmt.Errorf("incomplete PKI pair %s / %s; restore the missing file, existing keys are never replaced", cert, key)
	}
	return c, nil
}

func writeExclusive(path string, data []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func keyPEM(key crypto.Signer) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

func serial() (*big.Int, error) {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return n.Add(n, big.NewInt(1)), nil
}

func template(name string, lifetime time.Duration) (*x509.Certificate, error) {
	n, err := serial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &x509.Certificate{SerialNumber: n, Subject: pkix.Name{CommonName: name}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(lifetime), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature}, nil
}

func writePair(certPath, keyPath string, cert *x509.Certificate, parent *x509.Certificate, key, issuer crypto.Signer, chain ...[]byte) error {
	der, err := x509.CreateCertificate(rand.Reader, cert, parent, key.Public(), issuer)
	if err != nil {
		return err
	}
	encoded, err := keyPEM(key)
	if err != nil {
		return err
	}
	if err := writeExclusive(keyPath, encoded); err != nil {
		return err
	}
	b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	for _, extra := range chain {
		b = append(b, extra...)
	}
	return writeExclusive(certPath, b)
}

func loadCA(certPath, keyPath string) (*x509.Certificate, crypto.Signer, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	key, ok := pair.PrivateKey.(crypto.Signer)
	if !ok {
		return nil, nil, errors.New("CA key is not a signer")
	}
	if !cert.IsCA || !cert.BasicConstraintsValid || cert.KeyUsage&x509.KeyUsageCertSign == 0 {
		return nil, nil, errors.New("issuer certificate is not a signing CA")
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return nil, nil, errors.New("CA certificate is not currently valid")
	}
	return cert, key, nil
}

// Ensure initializes missing managed credentials, but reuses and verifies existing
// files. A partial, corrupt or expired CA is an error, never a reason to rotate it.
func Ensure(dir string, names []string) (Files, error) {
	files := ManagedFiles(dir)
	unlock, err := lock(dir)
	if err != nil {
		return files, err
	}
	defer unlock()
	caExists, err := pairState(files.CA, files.CAKey)
	if err != nil {
		return files, err
	}
	serverExists, err := pairState(files.Certificate, files.Key)
	if err != nil {
		return files, err
	}
	if !caExists && serverExists {
		return files, errors.New("managed CA missing while server credentials exist; restore the CA")
	}
	if !caExists {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return files, err
		}
		cert, err := template("adbgroup local CA", 10*365*24*time.Hour)
		if err != nil {
			return files, err
		}
		cert.IsCA = true
		cert.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		cert.MaxPathLen = 0
		cert.MaxPathLenZero = true
		if err := writePair(files.CA, files.CAKey, cert, cert, key, key); err != nil {
			return files, err
		}
	}
	ca, signer, err := loadCA(files.CA, files.CAKey)
	if err != nil {
		return files, err
	}
	if !serverExists {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return files, err
		}
		cert, err := template("adbgroup API", 2*365*24*time.Hour)
		if err != nil {
			return files, err
		}
		if cert.NotAfter.After(ca.NotAfter) {
			cert.NotAfter = ca.NotAfter
		}
		cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		for _, name := range names {
			if ip := net.ParseIP(name); ip != nil {
				cert.IPAddresses = append(cert.IPAddresses, ip)
			} else {
				cert.DNSNames = append(cert.DNSNames, name)
			}
		}
		if len(cert.IPAddresses)+len(cert.DNSNames) == 0 {
			return files, errors.New("server SANs are required")
		}
		if err := writePair(files.Certificate, files.Key, cert, ca, key, signer); err != nil {
			return files, err
		}
	}
	pair, err := tls.LoadX509KeyPair(files.Certificate, files.Key)
	if err != nil {
		return files, err
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return files, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	_, err = cert.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
	return files, err
}

func UserIdentity(user string) (string, error) {
	if len(user) == 0 || len(user) > 64 {
		return "", errors.New("user must contain 1 to 64 letters, digits, '.', '_' or '-'")
	}
	for _, c := range user {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '.' || c == '_' || c == '-') {
			return "", errors.New("invalid user name")
		}
	}
	return "spiffe://adbgroup/" + user, nil
}

func IdentityURI(identity string) (*url.URL, error) {
	u, err := url.Parse(identity)
	if err != nil || !u.IsAbs() || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(identity, "\r\n\t ") {
		return nil, errors.New("identity must be an absolute URI with a host and no credentials, query or fragment")
	}
	return u, nil
}

// VerifyIssuerTrust catches accidentally signing with a CA that the API does
// not trust. The signer bundle may start with an intermediate certificate.
func VerifyIssuerTrust(issuerPath, trustPath string) error {
	b, err := os.ReadFile(issuerPath)
	if err != nil {
		return err
	}
	var certificates []*x509.Certificate
	for len(b) > 0 {
		block, rest := pem.Decode(b)
		if block == nil || block.Type != "CERTIFICATE" {
			return errors.New("issuer bundle must contain only PEM certificates")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return err
		}
		certificates = append(certificates, cert)
		b = []byte(strings.TrimSpace(string(rest)))
	}
	if len(certificates) == 0 {
		return errors.New("no issuer certificate")
	}
	trusted, err := os.ReadFile(trustPath)
	if err != nil {
		return err
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(trusted) {
		return errors.New("no trusted client CA certificates")
	}
	intermediates := x509.NewCertPool()
	for _, cert := range certificates[1:] {
		intermediates.AddCert(cert)
	}
	_, err = certificates[0].Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})
	if err != nil {
		return fmt.Errorf("signing CA is not trusted by api.client_ca: %w", err)
	}
	return nil
}

// IssueClient creates a new export directory with client.pem, client.key and
// ca.pem. Existing output directories are rejected instead of overwriting keys.
func IssueClient(caPath, caKeyPath, identity, out string, days int) (Files, error) {
	var files Files
	uri, err := IdentityURI(identity)
	if err != nil {
		return files, err
	}
	if days < 1 || days > 3650 {
		return files, errors.New("days must be between 1 and 3650")
	}
	if out == "" {
		return files, errors.New("client output directory is required")
	}
	out = filepath.Clean(out)
	ca, signer, err := loadCA(caPath, caKeyPath)
	if err != nil {
		return files, err
	}
	b, err := os.ReadFile(caPath)
	if err != nil {
		return files, err
	}
	var bundle []byte
	for len(b) > 0 {
		block, rest := pem.Decode(b)
		if block == nil || block.Type != "CERTIFICATE" {
			return files, errors.New("CA bundle must contain only PEM certificates")
		}
		if _, err := x509.ParseCertificate(block.Bytes); err != nil {
			return files, err
		}
		bundle = append(bundle, pem.EncodeToMemory(block)...)
		b = []byte(strings.TrimSpace(string(rest)))
	}
	if len(bundle) == 0 {
		return files, errors.New("CA bundle has no certificates")
	}
	cert, err := template(identity, time.Duration(days)*24*time.Hour)
	if err != nil {
		return files, err
	}
	if cert.NotAfter.After(ca.NotAfter) {
		return files, errors.New("requested client validity exceeds CA expiry")
	}
	cert.URIs = []*url.URL{uri}
	cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return files, err
	}
	if err := os.MkdirAll(filepath.Dir(out), 0700); err != nil {
		return files, err
	}
	if err := os.Mkdir(out, 0700); err != nil {
		return files, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(out)
		}
	}()
	files = Files{CA: filepath.Join(out, "ca.pem"), Certificate: filepath.Join(out, "client.pem"), Key: filepath.Join(out, "client.key")}
	if err := writePair(files.Certificate, files.Key, cert, ca, key, signer, bundle); err != nil {
		return files, err
	}
	// Export the issuer, never its private key. The administrator may use an
	// intermediate signer; include the remaining chain from the input bundle.
	if err := writeExclusive(files.CA, bundle); err != nil {
		return files, err
	}
	complete = true
	return files, nil
}
