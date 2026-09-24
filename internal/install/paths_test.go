package install

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestPathsAreAbsoluteAndInsideHome guards against path-assignment ordering
// bugs: every managed path must be absolute and inside the owner home, and
// a clean home must pass p.check().
func TestPathsAreAbsoluteAndInsideHome(t *testing.T) {
	home := t.TempDir()
	p, err := pathsFor(home, "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.GARMConfig != filepath.Join(p.Secrets, "garm-config.toml") {
		t.Fatalf("GARMConfig must live under secrets, got %s", p.GARMConfig)
	}
	for name, path := range map[string]string{
		"Root": p.Root, "Release": p.Release, "State": p.State, "ConfigDir": p.ConfigDir,
		"HostConfig": p.HostConfig, "GARMConfig": p.GARMConfig, "Record": p.Record,
		"Secrets": p.Secrets, "CLIHome": p.CLIHome, "CA": p.CA, "Certificate": p.Certificate,
		"Key": p.Key, "Plist": p.Plist,
	} {
		if !filepath.IsAbs(path) {
			t.Fatalf("%s must be absolute, got %q", name, path)
		}
		if !strings.HasPrefix(filepath.Clean(path), filepath.Clean(home)+string(filepath.Separator)) {
			t.Fatalf("%s must be inside the owner home: %s", name, path)
		}
	}
	if err := p.check(); err != nil {
		t.Fatalf("clean home must pass check: %v", err)
	}
}
