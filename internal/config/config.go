// Package config loads and validates the single host configuration used by
// the garm-orbstack helper, and the adapter configuration used by the Linux
// provider executable running inside the GARM controller container.
//
// The host configuration is a TOML file whose canonical location is
// ~/.config/garm-orbstack/host.toml. All mutations go through UpdateHost,
// which serializes writers with an exclusive flock on a persistent sibling
// lock file, validates the result and publishes it atomically.
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

// DefaultHostPath returns the canonical host configuration path for the
// OrbStack owner account. It is resolved to an absolute path.
func DefaultHostPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home directory: %w", err)
	}
	return filepath.Join(home, ".config", "garm-orbstack", "host.toml"), nil
}

// Image maps an immutable image ID to an approved, stopped template machine.
type Image struct {
	MachineID    string `toml:"machine_id"`
	ManifestPath string `toml:"manifest_path"`
	Arch         string `toml:"arch"`
}

// Flavor describes the per-clone resource envelope applied before boot.
type Flavor struct {
	CPUs      int   `toml:"cpus"`
	MemoryMiB int   `toml:"memory_mib"`
	DiskBytes int64 `toml:"disk_bytes"`
}

// Host is the host-side helper configuration, bound to exactly one GARM
// controller identity.
type Host struct {
	ControllerID     string
	OrbctlPath       string
	StateDir         string
	MaxInstances     int
	OperationTimeout time.Duration
	Images           map[string]Image
	Flavors          map[string]Flavor
}

// Adapter is the configuration of the Linux provider executable inside the
// GARM controller container. It reaches the host helper over restricted SSH.
type Adapter struct {
	Host             string
	User             string
	IdentityFile     string
	KnownHostsFile   string
	Port             int
	OperationTimeout time.Duration
}

// hostTOML mirrors Host with duration strings, because TOML has no duration
// type and we decode them explicitly rather than relying on decode magic.
type hostTOML struct {
	ControllerID     string            `toml:"controller_id"`
	OrbctlPath       string            `toml:"orbctl_path"`
	StateDir         string            `toml:"state_dir"`
	MaxInstances     int               `toml:"max_instances"`
	OperationTimeout string            `toml:"operation_timeout"`
	Images           map[string]Image  `toml:"images"`
	Flavors          map[string]Flavor `toml:"flavors"`
}

func (h hostTOML) toHost() (Host, error) {
	host := Host{
		ControllerID: h.ControllerID,
		OrbctlPath:   h.OrbctlPath,
		StateDir:     h.StateDir,
		MaxInstances: h.MaxInstances,
		Images:       h.Images,
		Flavors:      h.Flavors,
	}
	if h.OperationTimeout != "" {
		d, err := time.ParseDuration(h.OperationTimeout)
		if err != nil {
			return Host{}, fmt.Errorf("invalid operation_timeout %q: %w", h.OperationTimeout, err)
		}
		host.OperationTimeout = d
	}
	return host, nil
}

func (h Host) toTOML() hostTOML {
	out := hostTOML{
		ControllerID: h.ControllerID,
		OrbctlPath:   h.OrbctlPath,
		StateDir:     h.StateDir,
		MaxInstances: h.MaxInstances,
		Images:       h.Images,
		Flavors:      h.Flavors,
	}
	if h.OperationTimeout != 0 {
		out.OperationTimeout = h.OperationTimeout.String()
	}
	return out
}

type adapterTOML struct {
	Host             string `toml:"host"`
	Port             int    `toml:"port"`
	User             string `toml:"user"`
	IdentityFile     string `toml:"identity_file"`
	KnownHostsFile   string `toml:"known_hosts_file"`
	OperationTimeout string `toml:"operation_timeout"`
}

// LoadHost reads and validates the host configuration at path. Zero images
// are valid (installed but unprepared controller). Once an image is
// registered its manifest file must exist; a missing manifest is an error.
func LoadHost(path string) (Host, error) {
	return loadHostFile(path)
}

func loadHostFile(path string) (Host, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Host{}, fmt.Errorf("reading host config %s: %w", path, err)
	}
	var decoded hostTOML
	md, err := toml.Decode(string(raw), &decoded)
	if err != nil {
		return Host{}, fmt.Errorf("parsing host config %s: %w", path, err)
	}
	if len(md.Undecoded()) > 0 {
		keys := make([]string, 0, len(md.Undecoded()))
		for _, k := range md.Undecoded() {
			keys = append(keys, k.String())
		}
		return Host{}, fmt.Errorf("host config %s has unknown keys: %s", path, strings.Join(keys, ", "))
	}
	host, err := decoded.toHost()
	if err != nil {
		return Host{}, fmt.Errorf("host config %s: %w", path, err)
	}
	if err := validateHost(host, path); err != nil {
		return Host{}, err
	}
	return host, nil
}

// LoadAdapter reads and validates the Linux adapter configuration.
func LoadAdapter(path string) (Adapter, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Adapter{}, fmt.Errorf("reading adapter config %s: %w", path, err)
	}
	var decoded adapterTOML
	md, err := toml.Decode(string(raw), &decoded)
	if err != nil {
		return Adapter{}, fmt.Errorf("parsing adapter config %s: %w", path, err)
	}
	if len(md.Undecoded()) > 0 {
		keys := make([]string, 0, len(md.Undecoded()))
		for _, k := range md.Undecoded() {
			keys = append(keys, k.String())
		}
		return Adapter{}, fmt.Errorf("adapter config %s has unknown keys: %s", path, strings.Join(keys, ", "))
	}
	adapter := Adapter{
		Host:           decoded.Host,
		User:           decoded.User,
		IdentityFile:   decoded.IdentityFile,
		KnownHostsFile: decoded.KnownHostsFile,
		Port:           decoded.Port,
	}
	if decoded.OperationTimeout != "" {
		d, err := time.ParseDuration(decoded.OperationTimeout)
		if err != nil {
			return Adapter{}, fmt.Errorf("adapter config %s: invalid operation_timeout %q: %w", path, decoded.OperationTimeout, err)
		}
		adapter.OperationTimeout = d
	}
	if err := validateAdapter(adapter, path); err != nil {
		return Adapter{}, err
	}
	return adapter, nil
}

func validateAdapter(a Adapter, path string) error {
	if strings.TrimSpace(a.Host) == "" {
		return fmt.Errorf("adapter config %s: host is required", path)
	}
	if a.Port <= 0 || a.Port > 65535 {
		return fmt.Errorf("adapter config %s: port must be in 1-65535, got %d", path, a.Port)
	}
	if strings.TrimSpace(a.User) == "" {
		return fmt.Errorf("adapter config %s: user is required", path)
	}
	if !filepath.IsAbs(a.IdentityFile) {
		return fmt.Errorf("adapter config %s: identity_file must be an absolute path", path)
	}
	if !filepath.IsAbs(a.KnownHostsFile) {
		return fmt.Errorf("adapter config %s: known_hosts_file must be an absolute path", path)
	}
	if a.OperationTimeout <= 0 {
		return fmt.Errorf("adapter config %s: operation_timeout must be positive", path)
	}
	return nil
}

func validateHost(h Host, path string) error {
	if _, err := uuid.Parse(h.ControllerID); err != nil {
		return fmt.Errorf("host config %s: controller_id must be a UUID: %w", path, err)
	}
	if !filepath.IsAbs(h.OrbctlPath) || strings.TrimSpace(h.OrbctlPath) == "" {
		return fmt.Errorf("host config %s: orbctl_path must be an absolute path", path)
	}
	if !filepath.IsAbs(h.StateDir) || strings.TrimSpace(h.StateDir) == "" {
		return fmt.Errorf("host config %s: state_dir must be an absolute path", path)
	}
	if h.MaxInstances < 1 {
		return fmt.Errorf("host config %s: max_instances must be >= 1", path)
	}
	if h.OperationTimeout <= 0 {
		return fmt.Errorf("host config %s: operation_timeout must be positive", path)
	}
	if len(h.Flavors) == 0 {
		return fmt.Errorf("host config %s: at least one flavor is required", path)
	}
	for id, img := range h.Images {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("host config %s: image id must not be empty", path)
		}
		if strings.TrimSpace(img.MachineID) == "" {
			return fmt.Errorf("host config %s: image %q: machine_id is required", path, id)
		}
		if !filepath.IsAbs(img.ManifestPath) || strings.TrimSpace(img.ManifestPath) == "" {
			return fmt.Errorf("host config %s: image %q: manifest_path must be an absolute path", path, id)
		}
		if _, err := os.Stat(img.ManifestPath); err != nil {
			return fmt.Errorf("host config %s: image %q: manifest %s: %w", path, id, img.ManifestPath, err)
		}
		if img.Arch != "arm64" && img.Arch != "amd64" {
			return fmt.Errorf("host config %s: image %q: arch must be arm64 or amd64, got %q", path, id, img.Arch)
		}
	}
	for id, fl := range h.Flavors {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("host config %s: flavor id must not be empty", path)
		}
		if fl.CPUs < 1 {
			return fmt.Errorf("host config %s: flavor %q: cpus must be >= 1", path, id)
		}
		if fl.MemoryMiB < 1 {
			return fmt.Errorf("host config %s: flavor %q: memory_mib must be >= 1", path, id)
		}
		if fl.DiskBytes < 1 {
			return fmt.Errorf("host config %s: flavor %q: disk_bytes must be >= 1", path, id)
		}
	}
	return nil
}

// UpdateHost is the sole writer of the host configuration. It takes an
// exclusive flock on the persistent sibling lock file "host.toml.lock",
// loads the existing configuration (a zero Host is used only when the file
// does not exist, i.e. initial creation), allocates the Images map only if
// nil while preserving all existing entries, applies the update callback,
// validates the result and publishes it atomically via a mode-0600 temporary
// sibling followed by rename and a parent-directory fsync. Callback or
// validation failures never publish a change. The lock file is never
// truncated or removed.
func UpdateHost(path string, update func(*Host) error) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("host config path must be absolute: %s", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("creating host config directory: %w", err)
	}
	lockPath := path + ".lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("opening host config lock %s: %w", lockPath, err)
	}
	defer lockFile.Close()
	if err := unix.Flock(int(lockFile.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("locking host config lock %s: %w", lockPath, err)
	}
	defer func() {
		_ = unix.Flock(int(lockFile.Fd()), unix.LOCK_UN)
	}()

	host, err := loadHostFile(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		// Initial creation only: a zero Host. loadHostFile already wrapped
		// the not-exist error, so re-check the underlying file.
		if _, statErr := os.Stat(path); statErr == nil {
			return err
		}
		host = Host{}
	}
	if host.Images == nil {
		host.Images = map[string]Image{}
	}
	if err := update(&host); err != nil {
		return fmt.Errorf("host config update callback: %w", err)
	}
	if err := validateHost(host, path); err != nil {
		return err
	}
	return writeHostAtomic(path, host)
}

func writeHostAtomic(path string, host Host) error {
	var buf strings.Builder
	enc := toml.NewEncoder(&buf)
	if err := enc.Encode(host.toTOML()); err != nil {
		return fmt.Errorf("encoding host config: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".host.toml.*")
	if err != nil {
		return fmt.Errorf("creating host config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		if tmpName != "" {
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting host config permissions: %w", err)
	}
	if _, err := tmp.WriteString(buf.String()); err != nil {
		tmp.Close()
		return fmt.Errorf("writing host config temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("syncing host config temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing host config temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publishing host config: %w", err)
	}
	tmpName = ""
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("opening host config directory for fsync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("fsyncing host config directory: %w", err)
	}
	return nil
}
