package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/WaferJay/adbgroup/internal/gateway"
	"github.com/WaferJay/adbgroup/internal/pki"
)

// A canonical lower-case user gets a readable name. Other URIs get their own
// namespace, including names that would collide on case-insensitive volumes.
func identityExportName(identity string) string {
	user := strings.TrimPrefix(identity, "spiffe://adbgroup/")
	if uri, err := pki.UserIdentity(user); err == nil && uri == identity && user == strings.ToLower(user) && validProfileName(user) && !strings.HasPrefix(user, ".") {
		return user
	}
	return fmt.Sprintf("uri=%x", sha256.Sum256([]byte(identity)))
}

func ensureStartupPacks(cfg gateway.Config) error {
	if !cfg.PKI.AutoTLS || len(cfg.Identities) == 0 {
		return nil
	}
	root := filepath.Join(cfg.PKI.Directory, "clients")
	unlock, err := lockPrivateDir(root)
	if err != nil {
		return err
	}
	defer unlock()
	ca := pki.ManagedFiles(cfg.PKI.Directory)
	currentCA, err := readLimitedFile(ca.CA, maxPackEntry)
	if err != nil {
		return err
	}
	for _, identity := range slices.Sorted(maps.Keys(cfg.Identities)) {
		dir := filepath.Join(root, identityExportName(identity))
		if err := ensureIdentityPack(cfg, ca, currentCA, identity, dir); err != nil {
			return fmt.Errorf("provision client pack for %q in %s: %w", identity, dir, err)
		}
	}
	return nil
}

func ensureIdentityPack(cfg gateway.Config, ca pki.Files, currentCA []byte, identity, dir string) error {
	path := filepath.Join(dir, "client.adgpack")
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		if err := issueClientPack(cfg, ca.CA, ca.CAKey, identity, dir, 365, "", ""); err != nil {
			return err
		}
		slog.Info("issued client pack", "identity", identity, "path", path)
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return errors.New("expected a private export directory")
	}
	for _, name := range []string{"client.pem", "client.key", "ca.pem"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
			return fmt.Errorf("expected a private regular file: %s", name)
		}
	}
	c, err := readCredentials(filepath.Join(dir, "client.pem"), filepath.Join(dir, "client.key"), filepath.Join(dir, "ca.pem"))
	if err != nil {
		return err
	}
	if !bytes.Equal(currentCA, c.ca) {
		return errors.New("existing export belongs to a different CA")
	}
	chain, err := certificates(c.cert)
	if err != nil {
		return err
	}
	if len(chain[0].URIs) != 1 || chain[0].URIs[0].String() != identity {
		return errors.New("existing certificate has a different identity")
	}
	info, err = os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := writeClientPack(cfg, dir, path, "", ""); err != nil {
			return err
		}
		slog.Info("restored client pack", "identity", identity, "path", path)
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return errors.New("expected a private regular client.adgpack")
	}
	_, packed, err := readClientPack(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(packed.cert, c.cert) || !bytes.Equal(packed.key, c.key) || !bytes.Equal(packed.ca, c.ca) {
		return errors.New("existing pack differs from the exported PEM files")
	}
	return nil
}
