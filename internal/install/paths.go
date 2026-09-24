// Package install manages a repository-free, user-scoped native GARM installation.
package install

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	serviceLabel  = "dev.orbstack.garm"
	controllerURL = "https://localhost:9997"
	guestURL      = "https://host.orb.internal:9997"
)

type Paths struct {
	Home, Root, Release, State, ConfigDir, HostConfig, GARMConfig string
	Secrets, CLIHome, CA, Certificate, Key, Plist, Link, Record   string
}

func pathsFor(home, hostConfig, version string) (Paths, error) {
	if !filepath.IsAbs(home) || filepath.Clean(home) == string(filepath.Separator) {
		return Paths{}, errors.New("owner home must be an absolute, non-root directory")
	}
	if !validVersion(version) {
		return Paths{}, errors.New("invalid release version")
	}
	home = filepath.Clean(home)
	p := Paths{Home: home}
	p.Root = filepath.Join(home, ".local/share/garm-orbstack")
	p.Release = filepath.Join(p.Root, "releases", version)
	p.State = filepath.Join(p.Root, "state")
	p.ConfigDir = filepath.Join(home, ".config/garm-orbstack")
	p.HostConfig = filepath.Join(p.ConfigDir, "host.toml")
	if hostConfig != "" {
		if !filepath.IsAbs(hostConfig) || filepath.Clean(hostConfig) != p.HostConfig {
			return Paths{}, fmt.Errorf("--config must name the single managed host configuration %s", p.HostConfig)
		}
	}
	// The GARM config carries the JWT signing secret and database
	// passphrase in plaintext, so it lives under the managed secrets tree,
	// not the non-sensitive config directory.
	p.GARMConfig = filepath.Join(p.Secrets, "garm-config.toml")
	p.Record = filepath.Join(p.ConfigDir, "installation.json")
	p.Secrets = filepath.Join(home, ".config/secrets/garm-orbstack")
	p.CLIHome = filepath.Join(p.Secrets, "cli-home")
	p.CA = filepath.Join(p.Secrets, "ca.pem")
	p.Certificate = filepath.Join(p.Secrets, "server.pem")
	p.Key = filepath.Join(p.Secrets, "server-key.pem")
	p.Plist = filepath.Join(home, "Library/LaunchAgents", serviceLabel+".plist")
	p.Link = filepath.Join(home, ".local/bin/garm-orbstack")
	return p, nil
}

// rejectSymlinks checks every existing component without following a managed
// file or directory into unrelated data. The user's home itself may be a macOS
// volume alias, but nothing managed beneath it may be a symlink (except Link).
func rejectSymlinks(home, path string) error {
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path is outside owner home: %s", path)
	}
	cur := home
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symlink in managed path %s", cur)
		}
	}
	return nil
}

func (p Paths) check() error {
	for _, path := range []string{p.Release, p.State, p.ConfigDir, p.HostConfig, p.GARMConfig, p.Secrets, p.CLIHome, p.Plist, filepath.Dir(p.Link)} {
		if err := rejectSymlinks(p.Home, path); err != nil {
			return err
		}
	}
	return nil
}

func privateDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("not a real managed directory: %s", path)
	}
	return os.Chmod(path, 0o700)
}

func atomicFile(path string, content []byte, mode os.FileMode) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("refusing non-regular managed file %s", path)
		}
		old, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Equal(old, content) {
			return os.Chmod(path, mode)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".garm-install-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(content)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func copyNewFile(source, destination string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	return errors.Join(copyErr, out.Sync(), out.Close())
}
