package pki

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) Files {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "pki")
	f, err := Ensure(dir, []string{"localhost", "127.0.0.1", "adb.example"})
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func read(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func certificate(t *testing.T, path string) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(read(t, path))
	if block == nil {
		t.Fatal("no PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}
func roots(t *testing.T, path string) *x509.CertPool {
	t.Helper()
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(read(t, path)) {
		t.Fatal("no trust anchor")
	}
	return pool
}

func TestEnsureReusesKeysAndValidServerSANs(t *testing.T) {
	f := fixture(t)
	before := map[string][]byte{}
	for _, path := range []string{f.CA, f.CAKey, f.Certificate, f.Key} {
		before[path] = read(t, path)
	}
	if _, err := Ensure(filepath.Dir(f.CA), []string{"another.example"}); err != nil {
		t.Fatal(err)
	}
	for path, b := range before {
		if !bytes.Equal(b, read(t, path)) {
			t.Fatalf("existing credential replaced: %s", path)
		}
	}
	cert := certificate(t, f.Certificate)
	for _, name := range []string{"localhost", "127.0.0.1", "adb.example"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots(t, f.CA), DNSName: name, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
			t.Fatal(err)
		}
	}
	if cert.IsCA {
		t.Fatal("server certificate can sign certificates")
	}
	if err := cert.VerifyHostname("wrong.example"); err == nil {
		t.Fatal("unexpected hostname accepted")
	}
}

func TestIssueClientIdentityUsagePermissionsAndNoOverwrite(t *testing.T) {
	f := fixture(t)
	caBefore := read(t, f.CA)
	serverBefore := read(t, f.Certificate)
	out := filepath.Join(t.TempDir(), "alice")
	client, err := IssueClient(f.CA, f.CAKey, "spiffe://adbgroup/alice", out, 30)
	if err != nil {
		t.Fatal(err)
	}
	cert := certificate(t, client.Certificate)
	if cert.IsCA || len(cert.URIs) != 1 || cert.URIs[0].String() != "spiffe://adbgroup/alice" {
		t.Fatal("incorrect client identity or CA capability")
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots(t, f.CA), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal(err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots(t, f.CA), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err == nil {
		t.Fatal("client usable as a TLS server")
	}
	if _, err := tls.LoadX509KeyPair(client.Certificate, client.Key); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{client.CA, client.Certificate, client.Key} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("file permissions %s: %o", path, info.Mode().Perm())
		}
	}
	if info, err := os.Stat(out); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("client directory not private")
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !reflect.DeepEqual(names, []string{"ca.pem", "client.key", "client.pem"}) {
		t.Fatalf("unexpected exported files: %v", names)
	}
	keyBefore := read(t, client.Key)
	if _, err := IssueClient(f.CA, f.CAKey, "spiffe://adbgroup/alice", out, 30); err == nil {
		t.Fatal("existing export overwritten")
	}
	if !bytes.Equal(keyBefore, read(t, client.Key)) || !bytes.Equal(caBefore, read(t, f.CA)) || !bytes.Equal(serverBefore, read(t, f.Certificate)) {
		t.Fatal("issuing user certificate changed existing credentials")
	}
}

func TestEnsureRejectsPartialAndCorruptState(t *testing.T) {
	for _, which := range []string{"missing-ca-key", "missing-server-key", "missing-ca-pair", "corrupt-ca", "corrupt-server"} {
		t.Run(which, func(t *testing.T) {
			f := fixture(t)
			before := read(t, f.CA)
			switch which {
			case "missing-ca-key":
				os.Remove(f.CAKey)
			case "missing-server-key":
				os.Remove(f.Key)
			case "missing-ca-pair":
				os.Remove(f.CA)
				os.Remove(f.CAKey)
			case "corrupt-ca":
				os.WriteFile(f.CA, []byte("broken"), 0600)
			case "corrupt-server":
				os.WriteFile(f.Certificate, []byte("broken"), 0600)
			}
			if _, err := Ensure(filepath.Dir(f.CA), []string{"localhost"}); err == nil {
				t.Fatal("broken PKI was silently repaired/replaced")
			}
			if which != "missing-ca-pair" && which != "corrupt-ca" && !bytes.Equal(before, read(t, f.CA)) {
				t.Fatal("CA changed after invalid state")
			}
		})
	}
}

func TestConcurrentInitializationUsesOneCA(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "pki")
	var wg sync.WaitGroup
	failures := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := Ensure(dir, []string{"localhost"}); failures <- err }()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	f := ManagedFiles(dir)
	for _, user := range []string{"alice", "bob"} {
		out := filepath.Join(t.TempDir(), user)
		client, err := IssueClient(f.CA, f.CAKey, "spiffe://adbgroup/"+user, out, 30)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := certificate(t, client.Certificate).Verify(x509.VerifyOptions{Roots: roots(t, f.CA), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNamesAndIdentityValidation(t *testing.T) {
	names, err := ServerNames([]string{"adb.example", "192.0.2.42"}, "0.0.0.0:8443")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "::1", "adb.example", "192.0.2.42"} {
		found := false
		for _, got := range names {
			found = found || got == name
		}
		if !found {
			t.Fatalf("missing SAN %s", name)
		}
	}
	for _, name := range []string{"", "0.0.0.0", "https://adb.example", "bad name"} {
		if _, err := ServerNames([]string{name}, "127.0.0.1:8443"); err == nil {
			t.Fatalf("bad SAN accepted: %q", name)
		}
	}
	for _, identity := range []string{"alice", "/alice", "spiffe://adbgroup/alice?admin=true", "spiffe://user:password@adbgroup/alice", "spiffe://adbgroup/alice#x"} {
		if _, err := IdentityURI(identity); err == nil {
			t.Fatalf("invalid URI accepted: %q", identity)
		}
	}
	identity, err := UserIdentity("alice")
	if err != nil || identity != "spiffe://adbgroup/alice" {
		t.Fatal("incorrect user identity")
	}
	if _, err := UserIdentity("alice/../bob"); err == nil {
		t.Fatal("invalid username accepted")
	}
}

func TestIssueRejectsInvalidLifetimeAndSigner(t *testing.T) {
	f := fixture(t)
	for _, days := range []int{0, 3651, 3650} {
		out := filepath.Join(t.TempDir(), "client")
		if _, err := IssueClient(f.CA, f.CAKey, "spiffe://adbgroup/alice", out, days); err == nil {
			t.Fatalf("invalid validity accepted: %d", days)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("failed issuance left an export")
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	b, err := keyPEM(key)
	if err != nil {
		t.Fatal(err)
	}
	wrong := filepath.Join(t.TempDir(), "wrong.key")
	os.WriteFile(wrong, b, 0600)
	if _, err := IssueClient(f.CA, wrong, "spiffe://adbgroup/alice", filepath.Join(t.TempDir(), "client"), 30); err == nil {
		t.Fatal("mismatching issuer key accepted")
	}
}

func TestGeneratedCredentialsCompleteMTLSHandshake(t *testing.T) {
	f := fixture(t)
	client, err := IssueClient(f.CA, f.CAKey, "spiffe://adbgroup/alice", filepath.Join(t.TempDir(), "client"), 30)
	if err != nil {
		t.Fatal(err)
	}
	serverPair, err := tls.LoadX509KeyPair(f.Certificate, f.Key)
	if err != nil {
		t.Fatal(err)
	}
	clientPair, err := tls.LoadX509KeyPair(client.Certificate, client.Key)
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	a.SetDeadline(time.Now().Add(3 * time.Second))
	b.SetDeadline(time.Now().Add(3 * time.Second))
	server := tls.Server(a, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots(t, f.CA), SessionTicketsDisabled: true})
	peer := tls.Client(b, &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{clientPair}, RootCAs: roots(t, client.CA), ServerName: "adb.example"})
	result := make(chan error, 1)
	go func() { result <- server.Handshake() }()
	if err := peer.Handshake(); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	state := server.ConnectionState()
	if len(state.VerifiedChains) == 0 || len(state.PeerCertificates[0].URIs) != 1 || state.PeerCertificates[0].URIs[0].String() != "spiffe://adbgroup/alice" {
		t.Fatal("TLS did not authenticate expected URI identity")
	}
	if strings.Contains(string(read(t, client.CA)), "PRIVATE KEY") {
		t.Fatal("CA private key leaked to client")
	}
}

func TestIssuerMustBeTrustedByAPI(t *testing.T) {
	issuer := fixture(t)
	other := fixture(t)
	if err := VerifyIssuerTrust(issuer.CA, issuer.CA); err != nil {
		t.Fatal(err)
	}
	if err := VerifyIssuerTrust(other.CA, issuer.CA); err == nil {
		t.Fatal("untrusted issuer accepted")
	}
}
