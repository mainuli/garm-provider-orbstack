package install

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestInstalledVersionCannotBeReplacedWithDifferentBytes(t *testing.T) {
	// placeRelease deliberately uses the actual host architecture. The fixture
	// is remapped only when this test runs on an amd64 development machine.
	dir, sums := stageFixture(t)
	if runtime.GOARCH == "amd64" {
		for rel, old := range executableAssets("v1.0.0", "arm64") {
			name := executableAssets("v1.0.0", "amd64")[rel]
			if err := os.Rename(filepath.Join(dir, old), filepath.Join(dir, name)); err != nil {
				t.Fatal(err)
			}
			sums[name] = sums[old]
			delete(sums, old)
		}
		writeFixtureSums(t, dir, sums)
	}
	p, err := pathsFor(t.TempDir(), "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	release, err := verifyRelease(dir, "v1.0.0", runtime.GOARCH, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := placeRelease(p, release); err != nil {
		t.Fatal(err)
	}
	if err := placeRelease(p, release); err != nil {
		t.Fatalf("identical reinstall: %v", err)
	}
	installed, err := os.ReadFile(filepath.Join(p.Release, "garm"))
	if err != nil {
		t.Fatal(err)
	}
	asset := executableAssets("v1.0.0", runtime.GOARCH)["garm"]
	replacement := []byte("different published bytes under the same version")
	if err := os.WriteFile(filepath.Join(dir, asset), replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(replacement)
	sums[asset] = hex.EncodeToString(digest[:])
	writeFixtureSums(t, dir, sums)
	republished, err := verifyRelease(dir, "v1.0.0", runtime.GOARCH, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := placeRelease(p, republished); err == nil {
		t.Fatal("same-version binary replacement succeeded")
	}
	retained, err := os.ReadFile(filepath.Join(p.Release, "garm"))
	if err != nil || !bytes.Equal(retained, installed) {
		t.Fatal("changed installed bytes on refusal")
	}
}

func TestControllerBundleMustReachRunnersUnchanged(t *testing.T) {
	p, err := pathsFor(t.TempDir(), "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p.Secrets, 0o700); err != nil {
		t.Fatal(err)
	}
	ca := []byte("installation public CA\n")
	if err := os.WriteFile(p.CA, ca, 0o600); err != nil {
		t.Fatal(err)
	}
	info := controllerInfo{CA: ca, MetadataURL: guestURL + "/api/v1/metadata", CallbackURL: guestURL + "/api/v1/callbacks"}
	if err := validateControllerCA(p, info); err != nil {
		t.Fatal(err)
	}
	info.CA = nil
	if err := validateControllerCA(p, info); err == nil {
		t.Fatal("accepted controller without runner CA propagation")
	}
	info.CA = ca
	info.MetadataURL = controllerURL + "/api/v1/metadata"
	if err := validateControllerCA(p, info); err == nil {
		t.Fatal("accepted runner callback route to guest localhost")
	}
}

func TestLaunchPIDRefusesAmbiguousRunningService(t *testing.T) {
	pid, err := launchPID([]byte("state = running\n\tpid = 1234\n"))
	if err != nil || pid != 1234 {
		t.Fatalf("cannot observe service identity: %d %v", pid, err)
	}
	for _, status := range []string{"state = running\n", "state = running\npid = zero", "pid = -1"} {
		if _, err := launchPID([]byte(status)); err == nil {
			t.Fatalf("accepted ambiguous PID: %q", status)
		}
	}
	pid, err = launchPID([]byte("state = not running\n"))
	if err != nil || pid != 0 {
		t.Fatalf("stopped service was not recognized: %d %v", pid, err)
	}
}
