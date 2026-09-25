package templates

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
)

var imageIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,160}$`)
var runnerVersionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

func ValidSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validateManifest(m Manifest) error {
	if m.SchemaVersion != SchemaVersion {
		return errors.New("unsupported template manifest schema")
	}
	if !imageIDPattern.MatchString(m.ImageID) || strings.TrimSpace(m.MachineID) == "" || strings.HasPrefix(m.MachineID, "-") || strings.ContainsAny(m.MachineID, "/\x00\r\n") {
		return errors.New("invalid template image or machine identity")
	}
	if m.Arch != "arm64" && m.Arch != "amd64" {
		return errors.New("invalid template architecture")
	}
	if !ValidSHA256(m.RecipeSHA256) || !ValidSHA256(m.RunnerSHA256) {
		return errors.New("template manifest requires SHA-256 recipe and runner checksums")
	}
	arch := m.Arch
	if arch == "amd64" {
		arch = "x64"
	}
	prefix := "actions-runner-linux-" + arch + "-"
	if !strings.HasPrefix(m.RunnerFilename, prefix) || !strings.HasSuffix(m.RunnerFilename, ".tar.gz") {
		return errors.New("runner filename does not match template architecture")
	}
	version := strings.TrimSuffix(strings.TrimPrefix(m.RunnerFilename, prefix), ".tar.gz")
	if !runnerVersionPattern.MatchString(version) {
		return errors.New("runner archive version is not pinned")
	}
	if strings.TrimSpace(m.OSVersion) == "" || strings.ContainsAny(m.OSVersion, "\x00\r\n") {
		return errors.New("template manifest lacks the OrbStack-recorded OS version")
	}
	if m.Variant != "minimal" && m.Variant != "full" {
		return errors.New("template manifest variant must be minimal or full")
	}
	if strings.TrimSpace(m.OrbStackVersion) == "" || len(m.Packages) == 0 {
		return errors.New("template manifest lacks OrbStack or package provenance")
	}
	for name, version := range m.Packages {
		if strings.TrimSpace(name) == "" || strings.TrimSpace(version) == "" || strings.ContainsAny(name+version, "\x00\r\n") {
			return errors.New("invalid package version in template manifest")
		}
	}
	return nil
}

// LoadManifest is a read-only strict schema reader; missing, truncated, null or
// unknown-field manifests are failures, never silently repaired metadata.
func LoadManifest(path string) (Manifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return Manifest{}, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var manifest Manifest
	if err := dec.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decoding template manifest: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Manifest{}, errors.New("template manifest contains trailing data")
	}
	if err := validateManifest(manifest); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

// WriteManifest publishes a new immutable manifest without replacing an
// existing file. Linking the fsynced temporary file makes creation atomic.
func WriteManifest(path string, manifest Manifest) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".manifest-")
	if err != nil {
		return err
	}
	defer f.Close()
	defer os.Remove(f.Name())
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(manifest); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Link(f.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// ValidateMachine ensures only the stopped approved immutable template is used
// for cloning. It is shared by provider creation and template listing.
func ValidateMachine(manifest Manifest, machine orbstack.Machine) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	if machine.ID != manifest.MachineID || machine.State != orbstack.StateStopped {
		return errors.New("approved template must be present and stopped with its recorded ID")
	}
	if machine.Image.Distro != "ubuntu" || machine.Image.Version != manifest.OSVersion || machine.Image.Arch != manifest.Arch {
		return errors.New("template operating system does not match manifest")
	}
	if !machine.Config.Isolated || machine.Config.ForwardSSHAgent || machine.Config.IsolateNetwork {
		return errors.New("template isolation settings do not match the approved baseline")
	}
	return nil
}
