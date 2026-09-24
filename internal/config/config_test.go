package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeMinimalHost(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "host.toml")
	manifest := filepath.Join(dir, "manifest.json")
	os.WriteFile(manifest, []byte("{}"), 0o600)
	content := `
controller_id = "8f14e45f-ceea-4671-9e6b-3f7a1d2c4b5e"
orbctl_path = "/usr/local/bin/orbctl"
state_dir = "` + filepath.Join(dir, "state") + `"
max_instances = 2
operation_timeout = "2m"

[images.img-1]
machine_id = "01M32577JPNWC7R27KS12YK1ZV"
manifest_path = "` + manifest + `"
arch = "arm64"

[flavors.default]
cpus = 2
memory_mib = 4096
disk_bytes = 68719476736
`
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadHostRoundTrip(t *testing.T) {
	dir := t.TempDir()
	p := writeMinimalHost(t, dir)
	h, err := LoadHost(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if h.ControllerID != "8f14e45f-ceea-4671-9e6b-3f7a1d2c4b5e" || h.MaxInstances != 2 || h.OperationTimeout != 2*time.Minute {
		t.Fatalf("unexpected host: %+v", h)
	}
	if h.Images["img-1"].Arch != "arm64" || h.Flavors["default"].CPUs != 2 {
		t.Fatalf("unexpected maps: %+v %+v", h.Images, h.Flavors)
	}
}

func TestLoadHostRejectsUnknownKeys(t *testing.T) {
	dir := t.TempDir()
	p := writeMinimalHost(t, dir)
	content, _ := os.ReadFile(p)
	os.WriteFile(p, append(content, []byte("\nunknown_key = 1\n")...), 0o600)
	if _, err := LoadHost(p); err == nil {
		t.Fatal("expected unknown key rejection")
	}
}

func TestLoadHostZeroImagesValidMissingManifestError(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "host.toml")
	content := `
controller_id = "8f14e45f-ceea-4671-9e6b-3f7a1d2c4b5e"
orbctl_path = "/usr/local/bin/orbctl"
state_dir = "` + filepath.Join(dir, "state") + `"
max_instances = 1
operation_timeout = "1m"

[flavors.default]
cpus = 1
memory_mib = 1024
disk_bytes = 10737418240

[images.broken]
machine_id = "x"
manifest_path = "` + filepath.Join(dir, "missing.json") + `"
arch = "arm64"
`
	os.WriteFile(p, []byte(content), 0o600)
	if _, err := LoadHost(p); err == nil {
		t.Fatal("expected missing manifest error")
	}
	// zero images variant must pass
	os.WriteFile(p, []byte(content[:indexOf(content, "[images.broken]")]+"\n"), 0o600)
	h, err := LoadHost(p)
	if err != nil || len(h.Images) != 0 {
		t.Fatalf("zero images must be valid: %v %+v", err, h)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestUpdateHostInitialAndIncremental(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "host.toml")
	manifest := filepath.Join(dir, "manifest.json")
	os.WriteFile(manifest, []byte("{}"), 0o600)

	err := UpdateHost(p, func(h *Host) error {
		h.ControllerID = "8f14e45f-ceea-4671-9e6b-3f7a1d2c4b5e"
		h.OrbctlPath = "/usr/local/bin/orbctl"
		h.StateDir = filepath.Join(dir, "state")
		h.MaxInstances = 2
		h.OperationTimeout = 2 * time.Minute
		h.Flavors = map[string]Flavor{"default": {CPUs: 2, MemoryMiB: 4096, DiskBytes: 68719476736}}
		return nil
	})
	if err != nil {
		t.Fatalf("initial update: %v", err)
	}

	err = UpdateHost(p, func(h *Host) error {
		h.Images["img-1"] = Image{MachineID: "01ABC", ManifestPath: manifest, Arch: "arm64"}
		return nil
	})
	if err != nil {
		t.Fatalf("incremental update: %v", err)
	}
	h, err := LoadHost(p)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(h.Images) != 1 || h.Images["img-1"].MachineID != "01ABC" || len(h.Flavors) != 1 {
		t.Fatalf("lost state: %+v", h)
	}
	// callback failure must not publish
	err = UpdateHost(p, func(h *Host) error { return os.ErrPermission })
	if err == nil {
		t.Fatal("expected callback error")
	}
	h2, _ := LoadHost(p)
	if len(h2.Images) != 1 {
		t.Fatal("callback failure must not change file")
	}
	// conflicting mapping for same ID is caller-validated; identical is idempotent at load level
}

func TestUpdateHostPreservesUnrelatedImages(t *testing.T) {
	dir := t.TempDir()
	p := writeMinimalHost(t, dir)
	err := UpdateHost(p, func(h *Host) error {
		h.Images["img-2"] = Image{MachineID: "02DEF", ManifestPath: filepath.Join(dir, "manifest.json"), Arch: "amd64"}
		return nil
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	h, _ := LoadHost(p)
	if len(h.Images) != 2 {
		t.Fatalf("expected 2 images, got %+v", h.Images)
	}
}
