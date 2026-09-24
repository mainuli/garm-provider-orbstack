package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/cloudbase/garm-provider-common/params"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

func TestRunnerCacheMustMatchSelectedTools(t *testing.T) {
	manifest := templates.Manifest{RunnerFilename: "actions-runner-linux-arm64-2.333.0.tar.gz", RunnerSHA256: strings.Repeat("a", 64)}
	for _, test := range []struct {
		name, filename, checksum string
		matches                  bool
	}{
		{"matching", manifest.RunnerFilename, manifest.RunnerSHA256, true},
		{"checksum optional", manifest.RunnerFilename, "", true},
		{"case insensitive digest", manifest.RunnerFilename, strings.Repeat("A", 64), true},
		{"version changed", "actions-runner-linux-arm64-2.334.0.tar.gz", manifest.RunnerSHA256, false},
		{"checksum changed", manifest.RunnerFilename, strings.Repeat("b", 64), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			tools := params.RunnerApplicationDownload{Filename: new(test.filename), SHA256Checksum: new(test.checksum)}
			if got := cacheMatches(manifest, tools); got != test.matches {
				t.Fatalf("cache compatibility = %v, want %v", got, test.matches)
			}
		})
	}
}

func TestUnsupportedBootstrapOptionsFailBeforeClone(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*params.BootstrapInstance)
	}{
		{"ssh key", func(b *params.BootstrapInstance) { b.SSHKeys = []string{"ssh-ed25519 public"} }},
		{"host path", func(b *params.BootstrapInstance) { b.ExtraSpecs = json.RawMessage(`{"host_path":"/private"}`) }},
		{"isolation override", func(b *params.BootstrapInstance) { b.ExtraSpecs = json.RawMessage(`{"isolated":false}`) }},
		{"null extras", func(b *params.BootstrapInstance) { b.ExtraSpecs = json.RawMessage(`null`) }},
		{"unsafe script path", func(b *params.BootstrapInstance) {
			b.ExtraSpecs = json.RawMessage(`{"pre_install_scripts":{"../../escape":"ZWNobyBoaQ=="}}`)
		}},
		{"package option", func(b *params.BootstrapInstance) {
			b.UserDataOptions.ExtraPackages = []string{"--allow-unauthenticated"}
		}},
		{"package shell", func(b *params.BootstrapInstance) { b.UserDataOptions.ExtraPackages = []string{"git;id"} }},
		{"invalid CA", func(b *params.BootstrapInstance) { b.CACertBundle = []byte("not a certificate") }},
		{"unsupported OS", func(b *params.BootstrapInstance) { b.OSType = params.Windows }},
		{"architecture mismatch", func(b *params.BootstrapInstance) { b.OSArch = params.Amd64 }},
		{"unknown image", func(b *params.BootstrapInstance) { b.Image = "unregistered" }},
		{"unknown flavor", func(b *params.BootstrapInstance) { b.Flavor = "unlimited" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p, machines, bootstrap := fixture(t)
			test.change(&bootstrap)
			if _, err := p.CreateInstance(t.Context(), bootstrap); err == nil {
				t.Fatal("unsupported bootstrap option accepted")
			} else if strings.Contains(err.Error(), bootstrap.InstanceToken) {
				t.Fatal("validation error leaked instance token")
			}
			if machines.clones != 0 {
				t.Fatal("invalid bootstrap cloned a machine")
			}
			records, err := p.registry.Snapshot(t.Context())
			if err != nil || len(records) != 0 {
				t.Fatalf("invalid bootstrap consumed capacity: %#v %v", records, err)
			}
		})
	}
}

func TestCustomUpstreamTemplateAndCacheFallback(t *testing.T) {
	p, _, bootstrap := fixture(t)
	manifest, err := templates.LoadManifest(p.host.Images[bootstrap.Image].ManifestPath)
	if err != nil {
		t.Fatal(err)
	}
	specs := map[string]any{
		"runner_install_template": []byte("#!/bin/sh\necho '{{ .ExtraContext.message }}'\n"),
		"extra_context":           map[string]string{"message": "approved customization"},
		"pre_install_scripts":     map[string][]byte{"10-setup.sh": []byte("#!/bin/sh\ntrue\n")},
	}
	bootstrap.ExtraSpecs, err = json.Marshal(specs)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap.JitConfigEnabled = true
	bootstrap.UserDataOptions.ExtraPackages = []string{"git", "jq=1.7.1-1ubuntu0.24.04.1"}
	bootstrap.Tools[0].Filename = new("actions-runner-linux-arm64-2.334.0.tar.gz")
	plan, err := prepareBootstrap(bootstrap, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.removeCache {
		t.Fatal("newly selected runner version reused stale preloaded agent")
	}
	// Exercise the actual upstream template engine, not a provider substitute.
	var installedScript []byte
	for _, file := range plan.files {
		if file.name == "runner-install.sh" {
			installedScript = file.data
		}
	}
	if string(installedScript) != "#!/bin/sh\necho 'approved customization'\n" {
		t.Fatalf("upstream customization was not preserved: %q", installedScript)
	}
}
