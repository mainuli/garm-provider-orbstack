package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
)

// TestVersionOutputIsBareTag pins the machine-readable contract consumed by
// install.sh and the installer's provenance check: the ENTIRE trimmed output
// of `garm-orbstack version` must equal releaseinfo.Version.
func TestVersionOutputIsBareTag(t *testing.T) {
	oldStdout := os.Stdout
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = w
	runErr := runVersion(nil)
	w.Close()
	os.Stdout = oldStdout
	if runErr != nil {
		t.Fatalf("runVersion: %v", runErr)
	}
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(r); err != nil {
		t.Fatal(err)
	}
	got := strings.TrimSpace(buf.String())
	if got != releaseinfo.Version {
		t.Fatalf("version output must be exactly %q, got %q", releaseinfo.Version, got)
	}
}
