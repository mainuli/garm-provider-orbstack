package templates

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
)

func validManifest() Manifest {
	return Manifest{SchemaVersion: SchemaVersion, ImageID: "ubuntu-24.04-arm64-2.333.0-unique", MachineID: "template-id", OSVersion: "noble", Variant: "minimal", RecipeSHA256: strings.Repeat("a", 64), Arch: "arm64", RunnerFilename: "actions-runner-linux-arm64-2.333.0.tar.gz", RunnerSHA256: strings.Repeat("b", 64), OrbStackVersion: "2.2.3", Packages: map[string]string{"git": "1:2.43.0"}}
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

// TestVariantValidation pins the full-variant contract: valid names, the
// arm64-only pin for the official toolset, and manifest-level rejection of
// unknown variants.
func TestVariantValidation(t *testing.T) {
	if err := (BuildOptions{Arch: "amd64", RunnerVersion: "2.337.0", RunnerSHA256: strings.Repeat("a", 64), Variant: "full"}).Validate(); err == nil {
		t.Fatal("full variant must be arm64-only while pinned to the arm64 toolset")
	}
	if err := (BuildOptions{Arch: "arm64", RunnerVersion: "2.337.0", RunnerSHA256: strings.Repeat("a", 64), Variant: "turbo"}).Validate(); err == nil {
		t.Fatal("unknown variant must be rejected")
	}
	if err := (BuildOptions{Arch: "arm64", RunnerVersion: "2.337.0", RunnerSHA256: strings.Repeat("a", 64), Variant: ""}).Validate(); err != nil {
		t.Fatalf("empty variant must default to minimal: %v", err)
	}
	base := validManifest()
	base.Variant = "ultimate"
	if err := validateManifest(base); err == nil {
		t.Fatal("manifest must reject unknown variant")
	}
	if err := validateManifest(validManifest()); err != nil {
		t.Fatalf("minimal manifest must validate: %v", err)
	}
	full := validManifest()
	full.Variant = "full"
	full.ImageID = "ubuntu-24.04-arm64-full-2.337.0-unique"
	if err := validateManifest(full); err != nil {
		t.Fatalf("full manifest must validate: %v", err)
	}
}

// TestRecipeHashCoversToolsetScript ensures the immutable recipe hash includes
// the full-variant installer, so toolset changes can never reuse an image ID.
func TestRecipeHashCoversToolsetScript(t *testing.T) {
	_, _, toolset, _, err := recipe()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(toolset, []byte("actions/runner-images")) {
		t.Fatal("toolset recipe script missing or wrong content")
	}
}

// TestLoadManifestV010ShapeWithoutVariant pins upgrade compatibility: schema-1
// manifests written by v0.1.0 (no variant key) must load as minimal variants
// so registered images keep working after a version change.
func TestLoadManifestV010ShapeWithoutVariant(t *testing.T) {
	dir := t.TempDir()
	legacy := `{"schema_version":1,"image_id":"ubuntu-24.04-arm64-2.333.0-abc","machine_id":"01M","os_version":"noble","recipe_sha256":"` + strings.Repeat("a", 64) + `","arch":"arm64","runner_filename":"actions-runner-linux-arm64-2.333.0.tar.gz","runner_sha256":"` + strings.Repeat("b", 64) + `","orbstack_version":"2.2.3","packages":{"git":"1:2.43.0"}}`
	path := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("v0.1.0-shaped manifest must load: %v", err)
	}
	if m.Variant != "minimal" {
		t.Fatalf("missing variant must normalize to minimal, got %q", m.Variant)
	}
}

// TestRemoveGuards pins the safety rules of template removal: unknown IDs
// refuse, in-use images refuse via the registry snapshot.
func TestRemoveGuards(t *testing.T) {
	dir := t.TempDir()
	home := t.TempDir()
	t.Setenv("HOME", home)
	manifestPath := filepath.Join(dir, "m.json")
	if err := os.WriteFile(manifestPath, []byte(`{"schema_version":1,"image_id":"img-a","machine_id":"01MACH","os_version":"noble","variant":"minimal","recipe_sha256":"`+strings.Repeat("a", 64)+`","arch":"arm64","runner_filename":"actions-runner-linux-arm64-2.333.0.tar.gz","runner_sha256":"`+strings.Repeat("b", 64)+`","orbstack_version":"2.2.3","packages":{"git":"1"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, ".config", "garm-orbstack", "host.toml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		t.Fatal(err)
	}
	writeHost := func(images map[string]string) {
		var b strings.Builder
		fmt.Fprintf(&b, "controller_id = \"8f14e45f-ceea-4671-9e6b-3f7a1d2c4b5e\"\norbctl_path = \"/bin/true\"\nstate_dir = %q\nmax_instances = 2\noperation_timeout = \"2m\"\n\n[flavors.default]\ncpus = 2\nmemory_mib = 4096\ndisk_bytes = 68719476736\n\n[images]\n", filepath.Join(dir, "state"))
		for id, machine := range images {
			fmt.Fprintf(&b, "  [images.%q]\n    machine_id = %q\n    manifest_path = %q\n    arch = \"arm64\"\n", id, machine, manifestPath)
		}
		if err := os.WriteFile(cfgPath, []byte(b.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeHost(map[string]string{"img-a": "01MACH"})
	if err := Remove(context.Background(), cfgPath, "nope"); err == nil {
		t.Fatal("unknown image must refuse")
	}
	if err := state.Initialize(context.Background(), filepath.Join(dir, "state")); err != nil {
		t.Fatal(err)
	}
	reg, err := state.Open(context.Background(), filepath.Join(dir, "state"), "8f14e45f-ceea-4671-9e6b-3f7a1d2c4b5e")
	if err != nil {
		t.Fatal(err)
	}
	record, reserved, err := reg.Reserve(context.Background(), state.Record{SchemaVersion: state.SchemaVersion, ControllerID: "8f14e45f-ceea-4671-9e6b-3f7a1d2c4b5e", PoolID: "p", RunnerName: "r1", MachineName: "m", ImageID: "img-a", Flavor: "default", Phase: state.PhaseReserved}, 4)
	if err != nil || !reserved {
		t.Fatalf("reserve: %v %v", err, reserved)
	}
	record.MachineID = "01R"
	record.Phase = state.PhaseCreated
	if err := reg.Save(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	// In-use image must refuse.
	if err := Remove(context.Background(), cfgPath, "img-a"); err == nil {
		t.Fatal("in-use image must refuse removal")
	}
	// Drain, then removal proceeds to the (unavailable here) machine
	// deletion and fails there rather than refusing on grounds.
	if err := reg.Remove(context.Background(), "r1"); err != nil {
		t.Fatal(err)
	}
	err = Remove(context.Background(), cfgPath, "img-a")
	if err == nil || !strings.Contains(err.Error(), "deleting template machine") {
		t.Fatalf("expected machine-deletion failure on this host, got: %v", err)
	}
}
