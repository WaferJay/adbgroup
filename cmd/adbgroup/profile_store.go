package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type profile struct {
	Server    string    `json:"server"`
	Listen    string    `json:"listen,omitempty"`
	Directory string    `json:"directory"`
	Identity  string    `json:"identity"`
	Expires   time.Time `json:"expires"`
	Trust     string    `json:"ca_sha256"`
	Digest    string    `json:"credentials_sha256"`
}

type profileIndex struct {
	Default  string             `json:"default,omitempty"`
	Profiles map[string]profile `json:"profiles"`
}

func readProfiles(root string) (profileIndex, error) {
	index := profileIndex{Profiles: make(map[string]profile)}
	data, err := os.ReadFile(filepath.Join(root, "profiles.json"))
	if errors.Is(err, os.ErrNotExist) {
		return index, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &index)
	}
	if err == nil && index.Profiles == nil {
		err = errors.New("missing profiles")
	}
	if err != nil {
		return index, fmt.Errorf("read profile index: %w", err)
	}
	return index, nil
}

func saveProfiles(root string, index profileIndex) error {
	f, err := os.CreateTemp(root, ".index-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err = json.NewEncoder(f).Encode(index); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(root, "profiles.json"))
}

func lockPrivateDir(root string) (func(), error) {
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(root)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm()&0077 != 0 {
		return nil, fmt.Errorf("credential storage must be a private directory: %s", root)
	}
	f, err := os.OpenFile(filepath.Join(root, ".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { f.Close() }, nil
}

func lookupProfile(root, name string) (string, tunnelFile, error) {
	index, err := readProfiles(root)
	if err != nil {
		return "", tunnelFile{}, err
	}
	if name == "" {
		name = index.Default
	}
	if name == "" {
		return "", tunnelFile{}, nil
	}
	p, ok := index.Profiles[name]
	if !ok {
		return "", tunnelFile{}, fmt.Errorf("unknown profile %q", name)
	}
	return name, tunnelFile{Server: p.Server, Listen: p.Listen, Credentials: filepath.Join(root, p.Directory)}, nil
}

func credentialDigest(c credentials) string {
	hash := sha256.New()
	for _, data := range [][]byte{c.cert, c.key, c.ca} {
		sum := sha256.Sum256(data)
		hash.Write(sum[:])
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func (c credentials) install(root string) (string, error) {
	dir, err := os.MkdirTemp(root, "credentials-*")
	if err != nil {
		return "", err
	}
	for name, data := range map[string][]byte{"client.pem": c.cert, "client.key": c.key, "ca.pem": c.ca} {
		if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			os.RemoveAll(dir)
			return "", err
		}
	}
	return filepath.Base(dir), nil
}
