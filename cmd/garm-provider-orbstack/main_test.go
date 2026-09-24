package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

const sdkController = "33145d47-d0ce-40f4-8e28-397b5dcf891c"

func sdkFixture(t *testing.T) (config.Host, string) {
	t.Helper()
	dir := t.TempDir()
	orb := filepath.Join(dir, "orbctl")
	// This protocol fixture exposes only a successful empty inventory. Any
	// unexpected lifecycle command fails rather than pretending it succeeded.
	if err := os.WriteFile(orb, []byte("#!/bin/sh\nif [ \"$1\" = list ]; then printf '[]\\n'; else exit 99; fi\n"), 0700); err != nil {
		t.Fatal(err)
	}
	host := config.Host{ControllerID: sdkController, StateDir: dir, OrbctlPath: orb, MaxInstances: 2, OperationTimeout: time.Second, Images: map[string]config.Image{}, Flavors: map[string]config.Flavor{"default": {CPUs: 2, MemoryMiB: 4096, DiskBytes: 68719476736}}}
	path := filepath.Join(dir, "host.toml")
	if err := config.UpdateHost(path, func(h *config.Host) error { *h = host; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := state.Initialize(t.Context(), dir); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"GARM_INTERFACE_VERSION": "v0.1.0", "GARM_COMMAND": "GetVersion", "GARM_CONTROLLER_ID": sdkController, "GARM_PROVIDER_CONFIG_FILE": path, "GARM_POOL_ID": "", "GARM_INSTANCE_ID": ""} {
		t.Setenv(key, value)
	}
	return host, path
}

func invokeSDK(t *testing.T, input []byte) (string, error) {
	t.Helper()
	stdin, err := os.CreateTemp(t.TempDir(), "stdin-")
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	if _, err := stdin.Write(input); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	stdout, err := os.CreateTemp(t.TempDir(), "stdout-")
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	oldIn, oldOut := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdin, stdout
	defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
	runErr := run(context.Background())
	if _, err := stdout.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	out, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

func TestSDKVersionEmptyListAndNotFoundExitSemantics(t *testing.T) {
	sdkFixture(t)
	for _, version := range []string{"v0.1.0", ""} {
		t.Setenv("GARM_INTERFACE_VERSION", version)
		out, err := invokeSDK(t, nil)
		if err != nil || out != releaseinfo.Version {
			t.Fatalf("version protocol = %q, %v", out, err)
		}
	}
	t.Setenv("GARM_COMMAND", "ListInstances")
	t.Setenv("GARM_POOL_ID", "opaque-scale-set-entity")
	out, err := invokeSDK(t, nil)
	if err != nil || out != "[]" {
		t.Fatalf("empty list protocol = %q, %v", out, err)
	}
	t.Setenv("GARM_COMMAND", "GetInstance")
	t.Setenv("GARM_POOL_ID", "")
	t.Setenv("GARM_INSTANCE_ID", "unknown")
	out, err = invokeSDK(t, nil)
	if out != "" || common.ResolveErrorToExitCode(err) != 30 {
		t.Fatalf("not-found contract = %q %v (exit %d)", out, err, common.ResolveErrorToExitCode(err))
	}
}

func TestSDKRejectsVersionAndControllerBeforeOrbStack(t *testing.T) {
	host, _ := sdkFixture(t)
	if err := os.Remove(host.OrbctlPath); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARM_INTERFACE_VERSION", "v0.1.1")
	if _, err := invokeSDK(t, nil); err == nil || !strings.Contains(err.Error(), "v0.1.0") {
		t.Fatalf("unsupported interface not rejected: %v", err)
	}
	t.Setenv("GARM_INTERFACE_VERSION", "v0.1.0")
	t.Setenv("GARM_CONTROLLER_ID", "908642ed-00ed-4af0-9d08-ae767d788c15")
	t.Setenv("GARM_COMMAND", "RemoveAllInstances")
	if _, err := invokeSDK(t, nil); err == nil || !strings.Contains(err.Error(), "controller identity") {
		t.Fatalf("foreign controller not rejected: %v", err)
	}
}

func TestSDKCreatePoolMismatchAndDuplicateExit(t *testing.T) {
	host, path := sdkFixture(t)
	manifest := templates.Manifest{SchemaVersion: 1, ImageID: "approved-image", MachineID: "template-id", OSVersion: "noble", Arch: "arm64", RunnerFilename: "actions-runner-linux-arm64-2.333.0.tar.gz", RunnerSHA256: strings.Repeat("a", 64), RecipeSHA256: strings.Repeat("b", 64), OrbStackVersion: "2.2.3", Packages: map[string]string{"git": "1:2.43.0"}}
	manifestPath := filepath.Join(host.StateDir, "manifest.json")
	if err := templates.WriteManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	if err := config.UpdateHost(path, func(h *config.Host) error {
		h.Images[manifest.ImageID] = config.Image{MachineID: manifest.MachineID, Arch: manifest.Arch, ManifestPath: manifestPath}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GARM_COMMAND", "CreateInstance")
	t.Setenv("GARM_POOL_ID", "opaque-scale-set-entity")
	bootstrap := params.BootstrapInstance{Name: "runner-one", PoolID: "different-pool", OSType: params.Linux, OSArch: params.Arm64, Image: manifest.ImageID, Flavor: "default", Tools: []params.RunnerApplicationDownload{{OS: new("linux"), Architecture: new("arm64"), Filename: new(manifest.RunnerFilename), DownloadURL: new("https://github.com/actions/runner/verified")}}}
	input, err := json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := invokeSDK(t, input); err == nil || !strings.Contains(err.Error(), "pool") {
		t.Fatalf("pool mismatch accepted: %v", err)
	}
	reg, err := state.Open(t.Context(), host.StateDir, sdkController)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Reserve(t.Context(), state.Record{SchemaVersion: 1, ControllerID: sdkController, PoolID: "already-owned-pool", RunnerName: bootstrap.Name, MachineName: "reserved-name", ImageID: bootstrap.Image, Flavor: bootstrap.Flavor, Phase: state.PhaseReserved}, 2); err != nil {
		t.Fatal(err)
	}
	bootstrap.PoolID = "opaque-scale-set-entity"
	input, err = json.Marshal(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	out, err := invokeSDK(t, input)
	if out != "" || common.ResolveErrorToExitCode(err) != 31 {
		t.Fatalf("duplicate contract = %q %v (exit %d)", out, err, common.ResolveErrorToExitCode(err))
	}
}
