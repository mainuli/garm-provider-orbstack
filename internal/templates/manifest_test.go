package templates

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
)

func validManifest() Manifest {
	return Manifest{SchemaVersion: SchemaVersion, ImageID: "ubuntu-24.04-arm64-2.333.0-unique", MachineID: "template-id", OSVersion: "noble", RecipeSHA256: strings.Repeat("a", 64), Arch: "arm64", RunnerFilename: "actions-runner-linux-arm64-2.333.0.tar.gz", RunnerSHA256: strings.Repeat("b", 64), OrbStackVersion: "2.2.3", Packages: map[string]string{"git": "1:2.43.0"}}
}

func TestManifestValidationAndImmutability(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	manifest := validManifest()
	if err := WriteManifest(path, manifest); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadManifest(path)
	if err != nil || !reflect.DeepEqual(loaded, manifest) {
		t.Fatalf("manifest round trip: %#v %v", loaded, err)
	}
	changed := manifest
	changed.MachineID = "replacement-id"
	if err := WriteManifest(path, changed); err == nil {
		t.Fatal("immutable manifest overwritten")
	}
	loaded, err = LoadManifest(path)
	if err != nil || loaded.MachineID != manifest.MachineID {
		t.Fatalf("immutable metadata replaced: %#v %v", loaded, err)
	}
	for _, test := range []struct {
		name   string
		change func(*Manifest)
	}{
		{"schema", func(m *Manifest) { m.SchemaVersion = 2 }},
		{"machine", func(m *Manifest) { m.MachineID = "" }},
		{"recipe checksum", func(m *Manifest) { m.RecipeSHA256 = "invalid" }},
		{"runner checksum", func(m *Manifest) { m.RunnerSHA256 = strings.Repeat("z", 64) }},
		{"archive architecture", func(m *Manifest) { m.Arch = "amd64" }},
		{"unpinned version", func(m *Manifest) { m.RunnerFilename = "actions-runner-linux-arm64-latest.tar.gz" }},
		{"package provenance", func(m *Manifest) { m.Packages = nil }},
		{"OrbStack provenance", func(m *Manifest) { m.OrbStackVersion = "" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			invalid := validManifest()
			test.change(&invalid)
			data, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			badPath := filepath.Join(t.TempDir(), "invalid.json")
			if err := os.WriteFile(badPath, data, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadManifest(badPath); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	for _, raw := range []string{"null", "{}", "[]", `{"unknown":1}`, "{} {}"} {
		badPath := filepath.Join(t.TempDir(), "invalid.json")
		if err := os.WriteFile(badPath, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := LoadManifest(badPath); err == nil {
			t.Fatalf("invalid JSON manifest accepted: %s", raw)
		}
	}
}

func TestTemplateMustRemainStoppedIsolatedAndMatching(t *testing.T) {
	manifest := validManifest()
	baseline := orbstack.Machine{ID: manifest.MachineID, State: orbstack.StateStopped, Image: orbstack.Image{Distro: "ubuntu", Version: "noble", Arch: "arm64"}, Config: orbstack.MachineConfig{Isolated: true}}
	if err := ValidateMachine(manifest, baseline); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*orbstack.Machine){
		func(m *orbstack.Machine) { m.ID = "foreign-id" },
		func(m *orbstack.Machine) { m.State = orbstack.StateRunning },
		func(m *orbstack.Machine) { m.Config.Isolated = false },
		func(m *orbstack.Machine) { m.Config.ForwardSSHAgent = true },
		func(m *orbstack.Machine) { m.Image.Arch = "amd64" },
	} {
		machine := baseline
		mutate(&machine)
		if err := ValidateMachine(manifest, machine); err == nil {
			t.Fatalf("unsafe template accepted: %#v", machine)
		}
	}
}

func TestConcurrentRegistrationPreservesImagesAndHostSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "host.toml")
	host := config.Host{ControllerID: "33145d47-d0ce-40f4-8e28-397b5dcf891c", OrbctlPath: "/absolute/orbctl", StateDir: dir, MaxInstances: 7, OperationTimeout: time.Minute, Flavors: map[string]config.Flavor{"custom": {CPUs: 3, MemoryMiB: 5120, DiskBytes: 9876543210}}, Images: map[string]config.Image{}}
	if err := config.UpdateHost(path, func(h *config.Host) error { *h = host; return nil }); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	failures := make(chan error, 2)
	for _, suffix := range []string{"one", "two"} {
		manifest := validManifest()
		manifest.ImageID += "-" + suffix
		manifest.MachineID += "-" + suffix
		manifestPath := filepath.Join(dir, suffix+".json")
		if err := WriteManifest(manifestPath, manifest); err != nil {
			t.Fatal(err)
		}
		wg.Go(func() { failures <- RegisterImage(path, host, manifest, manifestPath) })
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	loaded, err := config.LoadHost(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Images) != 2 || loaded.MaxInstances != 7 || loaded.OperationTimeout != host.OperationTimeout || !reflect.DeepEqual(loaded.Flavors, host.Flavors) {
		t.Fatalf("registration lost images or overwrote settings: %#v", loaded)
	}
	for id, image := range loaded.Images {
		manifest, err := LoadManifest(image.ManifestPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := RegisterImage(path, host, manifest, image.ManifestPath); err != nil {
			t.Fatalf("identical registration not idempotent: %v", err)
		}
		manifest.MachineID = "different-id"
		if err := RegisterImage(path, host, manifest, image.ManifestPath); err == nil {
			t.Fatalf("immutable image %s reassigned", id)
		}
	}
}

func TestInvalidBuildInputsDoNotAccessConfiguration(t *testing.T) {
	for _, options := range []BuildOptions{
		{Arch: "x86", RunnerVersion: "2.333.0", RunnerSHA256: strings.Repeat("a", 64)},
		{Arch: "arm64", RunnerVersion: "latest", RunnerSHA256: strings.Repeat("a", 64)},
		{Arch: "arm64", RunnerVersion: "2.333.0", RunnerSHA256: "unverified"},
	} {
		if _, err := Build(context.Background(), filepath.Join(t.TempDir(), "missing.toml"), options); err == nil || strings.Contains(err.Error(), "missing.toml") {
			t.Fatalf("invalid version/checksum not rejected before file access: %v", err)
		}
	}
}
