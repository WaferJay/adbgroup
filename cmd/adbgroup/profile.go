package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/WaferJay/adbgroup/internal/tunnel"
)

func validProfileName(s string) bool {
	return s != "" && len(s) <= 128 && s != "." && s != ".." && !strings.HasPrefix(s, "-") && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "/\\") && strings.IndexFunc(s, unicode.IsControl) < 0
}

func prompt(question string) (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return "", errors.New("interactive terminal required")
	}
	fmt.Fprint(os.Stderr, question)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(line), err
}

func chooseServer(out io.Writer, servers []string, explicit, previous string) (string, error) {
	raw := explicit
	if raw == "" && (len(servers) == 0 || slices.Contains(servers, previous)) {
		raw = previous
	}
	if raw == "" && len(servers) == 1 {
		raw = servers[0]
	}
	if raw == "" {
		for i, s := range servers {
			fmt.Fprintf(out, "%d) %s\n", i+1, s)
		}
		question := "Enter HTTPS server: "
		if len(servers) > 0 {
			question = "Choose server number: "
		}
		answer, err := prompt(question)
		if err != nil {
			return "", errors.New("specify the HTTPS origin with -server")
		}
		if len(servers) == 0 {
			raw = answer
		} else {
			n, err := strconv.Atoi(answer)
			if err != nil || n < 1 || n > len(servers) {
				return "", errors.New("invalid server selection")
			}
			raw = servers[n-1]
		}
	}
	u, err := origin(raw)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func runProfile(args []string, out io.Writer) error {
	top := newCommandFlags("profile")
	if err := top.Parse(args); err != nil {
		return err
	}
	args = top.Args()
	if len(args) == 0 {
		top.Usage()
		return errors.New("profile command required")
	}
	action := args[0]
	flags := newCommandFlags("profile " + action)
	var name, server, legacy string
	switch action {
	case "import":
		flags.StringVar(&name, "name", "", "local profile name")
		flags.StringVar(&legacy, "f", "", "import existing client YAML")
		fallthrough
	case "update":
		flags.StringVar(&server, "server", "", "HTTPS origin")
	case "list", "set-default":
	default:
		return fmt.Errorf("unknown profile command %q", action)
	}
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	path, err := defaultTunnelConfig()
	if err != nil {
		return err
	}
	root := filepath.Join(filepath.Dir(path), "profiles")
	switch action {
	case "list":
		if flags.NArg() != 0 {
			return errors.New("profile list takes no arguments")
		}
		return listProfiles(root, out)
	case "set-default":
		if flags.NArg() != 1 {
			return errors.New("profile set-default requires a name")
		}
		unlock, err := lockPrivateDir(root)
		if err != nil {
			return err
		}
		defer unlock()
		index, err := readProfiles(root)
		if err != nil {
			return err
		}
		if _, ok := index.Profiles[flags.Arg(0)]; !ok {
			return fmt.Errorf("unknown profile %q", flags.Arg(0))
		}
		index.Default = flags.Arg(0)
		return saveProfiles(root, index)
	}
	return importProfile(root, path, action, name, server, legacy, flags.Args(), out)
}

func listProfiles(root string, out io.Writer) error {
	index, err := readProfiles(root)
	if err != nil {
		return err
	}
	active := currentSelection()
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSERVER\tIDENTITY\tEXPIRES\tDEFAULT\tTERMINAL")
	for _, n := range slices.Sorted(maps.Keys(index.Profiles)) {
		p := index.Profiles[n]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%t\t%t\n", n, p.Server, p.Identity, p.Expires.UTC().Format(time.RFC3339), n == index.Default, active != nil && active.Profile == n)
	}
	return w.Flush()
}

func importProfile(root, path, action, name, server, legacy string, args []string, out io.Writer) error {
	var err error
	var c credentials
	var m packManifest
	var listen string
	if legacy != "" {
		if len(args) != 0 {
			return errors.New("provide either -f or a pack file")
		}
		cfg, found, err := loadTunnelFile(legacy)
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("missing config %s", legacy)
		}
		opts := tunnelOptions{credentials: cfg.Credentials, cert: cfg.Certificate, key: cfg.Key, ca: cfg.CA}
		c, err = readCredentials(opts.credentialFiles())
		if err != nil {
			return err
		}
		if cfg.Server == "" {
			cfg.Server = defaultTunnelServer
		}
		m = packManifest{1, strings.TrimSuffix(filepath.Base(legacy), filepath.Ext(legacy)), []string{cfg.Server}}
		listen = cfg.Listen
		if listen != "" {
			if _, err = tunnel.LocalAddress(listen); err != nil {
				return err
			}
		}
	} else {
		var pack string
		if action == "update" {
			if len(args) != 2 {
				return errors.New("profile update requires NAME PACK")
			}
			name, pack = args[0], args[1]
		} else if len(args) != 1 {
			return errors.New("profile import requires a pack file")
		} else {
			pack = args[0]
		}
		m, c, err = readClientPack(pack)
		if err != nil {
			return err
		}
	}
	if name == "" {
		name = m.Name
	}
	if !validProfileName(name) {
		return errors.New("invalid profile name; specify -name")
	}
	info, err := c.validate()
	if err != nil {
		return err
	}
	unlock, err := lockPrivateDir(root)
	if err != nil {
		return err
	}
	defer unlock()
	index, err := readProfiles(root)
	if err != nil {
		return err
	}
	old, exists := index.Profiles[name]
	if action == "update" && !exists {
		return fmt.Errorf("unknown profile %q", name)
	}
	selected, err := chooseServer(out, m.Servers, server, old.Server)
	if err != nil {
		return err
	}
	p := profile{
		Server: selected, Listen: listen, Identity: info.leaf.URIs[0].String(),
		Expires: info.leaf.NotAfter, Digest: credentialDigest(c), Trust: info.caFingerprints,
	}
	if exists {
		if action == "update" {
			p.Listen = old.Listen
		}
		if old.Server == p.Server && old.Digest == p.Digest && old.Listen == p.Listen {
			fmt.Fprintf(out, "Profile %s already configured\n", name)
			return nil
		}
		if action != "update" {
			return fmt.Errorf("profile %q exists; use profile update to renew or import -name to add another profile", name)
		}
		if p.Server != old.Server || p.Identity != old.Identity {
			return errors.New("gateway or identity changed; import under a new name")
		}
		if p.Trust != old.Trust {
			fmt.Fprintf(out, "Server CA fingerprints (SHA-256) changed\nOld: %s\nNew: %s\n", old.Trust, p.Trust)
			answer, err := prompt("Accept new CA (yes/no)? ")
			if err != nil || answer != "yes" {
				return errors.New("CA change was not confirmed")
			}
		}
	}
	if !exists && len(index.Profiles) == 0 {
		if _, err = os.Lstat(path); errors.Is(err, os.ErrNotExist) {
			index.Default = name
		} else if err != nil {
			return err
		}
	}
	p.Directory, err = c.install(root)
	if err != nil {
		return err
	}
	index.Profiles[name] = p
	if err = saveProfiles(root, index); err != nil {
		os.RemoveAll(filepath.Join(root, p.Directory))
		return err
	}
	fmt.Fprintf(out, "Profile %s configured for %s as %s (expires %s)\n", name, selected, p.Identity, p.Expires.UTC().Format(time.RFC3339))
	return nil
}
