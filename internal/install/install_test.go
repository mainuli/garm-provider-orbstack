package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
)

func TestDevelopmentInstallRejectsBeforeManagedState(t *testing.T) {
	version, source := releaseinfo.Version, releaseinfo.GARMSource
	releaseinfo.Version, releaseinfo.GARMSource = "dev", ""
	t.Cleanup(func() { releaseinfo.Version, releaseinfo.GARMSource = version, source })
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("PATH", "")
	if err := Install(context.Background(), Options{FromDir: filepath.Join(home, "does-not-exist")}); err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("expected metadata rejection, got %v", err)
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatal("development installer touched managed state")
	}
}

func TestPathPolicyRejectsTraversalAndUnmanagedConfig(t *testing.T) {
	home := t.TempDir()
	for _, version := range []string{"dev", "", "../v1.0.0", "v1.0.0/other", "v1..0"} {
		if _, err := pathsFor(home, "", version); err == nil {
			t.Fatalf("accepted version %q", version)
		}
	}
	if _, err := pathsFor("relative/home", "", "v1.0.0"); err == nil {
		t.Fatal("accepted relative home")
	}
	if _, err := pathsFor(home, filepath.Join(home, "unmanaged.toml"), "v1.0.0"); err == nil {
		t.Fatal("accepted noncanonical configuration")
	}
	p, err := pathsFor(home, "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if p.HostConfig != filepath.Join(home, ".config/garm-orbstack/host.toml") || !filepath.IsAbs(p.Release) {
		t.Fatal("paths are not rooted exclusively in the owner's home")
	}
	if err := os.Mkdir(filepath.Join(home, ".config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), p.ConfigDir); err != nil {
		t.Fatal(err)
	}
	if err := p.check(); err == nil {
		t.Fatal("accepted managed directory symlink")
	}
}

func TestManagedFileDoesNotOverwriteUnrelatedEdits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("operator changes"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := managedFile(path, []byte("new"), []byte("old")); err == nil {
		t.Fatal("overwrote unmanaged contents")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "operator changes" {
		t.Fatal("changed operator data on refusal")
	}
}

func stageFixture(t *testing.T) (string, map[string]string) {
	t.Helper()
	dir := t.TempDir()
	files := make(map[string][]byte)
	for _, asset := range executableAssets("v1.0.0", "arm64") {
		files[asset] = []byte("unique release bytes: " + asset)
	}
	lock := ReleaseLock{Version: "v1.0.0", GoVersion: "go1.26.2", CLIPatchSHA256: strings.Repeat("a", 64)}
	lock.GARMSource.Tag, lock.GARMSource.Commit = upstreamTag, upstreamCommit
	var err error
	files["release-lock.json"], err = json.Marshal(lock)
	if err != nil {
		t.Fatal(err)
	}
	sums := make(map[string]string)
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(data)
		sums[name] = hex.EncodeToString(digest[:])
	}
	writeFixtureSums(t, dir, sums)
	return dir, sums
}

func writeFixtureSums(t *testing.T, dir string, sums map[string]string) {
	t.Helper()
	var data strings.Builder
	for name, digest := range sums {
		fmt.Fprintf(&data, "%s  %s\n", digest, name)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(data.String()), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestStagingRejectsCorruptionMissingAndDuplicateChecksums(t *testing.T) {
	for _, change := range []string{"binary", "lock", "missing", "duplicate", "symlink", "unlisted", "source"} {
		t.Run(change, func(t *testing.T) {
			dir, sums := stageFixture(t)
			if _, err := verifyRelease(dir, "v1.0.0", "arm64", false); err != nil {
				t.Fatal(err)
			}
			asset := executableAssets("v1.0.0", "arm64")["garm"]
			switch change {
			case "binary":
				if err := os.WriteFile(filepath.Join(dir, asset), []byte("tampered"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "lock":
				if err := os.WriteFile(filepath.Join(dir, "release-lock.json"), []byte(`{"version":"v2.0.0"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing":
				delete(sums, asset)
				writeFixtureSums(t, dir, sums)
			case "duplicate":
				f, err := os.OpenFile(filepath.Join(dir, "SHA256SUMS"), os.O_APPEND|os.O_WRONLY, 0o600)
				if err != nil {
					t.Fatal(err)
				}
				_, err = fmt.Fprintf(f, "%s  %s\n", sums[asset], asset)
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(errors.Join(err, closeErr))
				}
			case "symlink":
				original := filepath.Join(t.TempDir(), "binary")
				if err := os.Rename(filepath.Join(dir, asset), original); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(original, filepath.Join(dir, asset)); err != nil {
					t.Fatal(err)
				}
			case "unlisted":
				if err := os.WriteFile(filepath.Join(dir, "extra"), []byte("not verified"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "source":
				path := filepath.Join(dir, "release-lock.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				data = bytes.ReplaceAll(data, []byte(upstreamCommit), []byte(strings.Repeat("b", 40)))
				if err := os.WriteFile(path, data, 0o600); err != nil {
					t.Fatal(err)
				}
				digest := sha256.Sum256(data)
				sums["release-lock.json"] = hex.EncodeToString(digest[:])
				writeFixtureSums(t, dir, sums)
			}
			if _, err := verifyRelease(dir, "v1.0.0", "arm64", false); err == nil {
				t.Fatalf("accepted %s release", change)
			}
		})
	}
}

func TestTLSNamesKeyPairAndRepeatPreservation(t *testing.T) {
	p, err := pathsFor(t.TempDir(), "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	first, err := ensureSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(p.CA)
	if err != nil {
		t.Fatal(err)
	}
	second, err := ensureSecrets(p)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(p.CA)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || !bytes.Equal(before, after) {
		t.Fatal("repeat install rotated persistent encryption identity")
	}
	pair, err := tls.LoadX509KeyPair(p.Certificate, p.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	roots, err := installationRoots(p.CA)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "host.orb.internal", "host.docker.internal"} {
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: name, CurrentTime: time.Now()}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: "foreign.example"}); err == nil {
		t.Fatal("certificate authenticated unrelated host")
	}
	keyPEM, err := os.ReadFile(p.Key)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(keyPEM)
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil || key.N.BitLen() != 3072 {
		t.Fatal("server key is not RSA 3072")
	}
	if err := os.Remove(p.Key); err != nil {
		t.Fatal(err)
	}
	if _, err := ensureSecrets(p); err == nil {
		t.Fatal("silently regenerated partial secrets")
	}
	retained, err := os.ReadFile(p.CA)
	if err != nil || !bytes.Equal(retained, before) {
		t.Fatal("mutated CA while refusing incomplete secrets")
	}
}

func TestGARMConfigConsumerContract(t *testing.T) {
	p, err := pathsFor(filepath.Join(t.TempDir(), "Owner & Space"), "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		API struct {
			Bind string
			Port int
			TLS  bool `toml:"use_tls"`
		} `toml:"apiserver"`
		JWT struct {
			Secret string
			TTL    string `toml:"time_to_live"`
		} `toml:"jwt_auth"`
		Database struct {
			Backend, Passphrase string
			SQLite              struct {
				DB string `toml:"db_file"`
			} `toml:"sqlite3"`
		} `toml:"database"`
		Providers []struct {
			Name     string
			Type     string `toml:"provider_type"`
			External struct {
				Binary  string `toml:"provider_executable"`
				Config  string `toml:"config_file"`
				Version string `toml:"interface_version"`
			} `toml:"external"`
		} `toml:"provider"`
	}
	secrets := credentials{"jwt-secret", "encryption-secret"}
	if _, err := toml.Decode(string(renderGARMConfig(p, secrets, true)), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.API.Bind != "127.0.0.1" || decoded.API.Port != 9997 || !decoded.API.TLS {
		t.Fatal("controller is not TLS-only on loopback")
	}
	if decoded.JWT.TTL != "8760h" || decoded.Database.SQLite.DB != filepath.Join(p.State, "garm.db") || decoded.Database.Passphrase != secrets.DatabasePassphrase {
		t.Fatal("database/JWT contract is invalid")
	}
	if len(decoded.Providers) != 1 || decoded.Providers[0].External.Config != p.HostConfig || decoded.Providers[0].External.Version != "v0.1.0" || !filepath.IsAbs(decoded.Providers[0].External.Binary) {
		t.Fatal("external provider config must use the nested absolute-path contract")
	}
	var initial map[string]any
	if _, err := toml.Decode(string(renderGARMConfig(p, secrets, false)), &initial); err != nil {
		t.Fatal(err)
	}
	if _, exists := initial["provider"]; exists {
		t.Fatal("unbound first boot enables provider")
	}
}

func TestLaunchAgentArgvEscapingAndNoServeSubcommand(t *testing.T) {
	p, err := pathsFor(filepath.Join(t.TempDir(), "Owner & <Space>"), "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	var plist struct {
		Dict struct {
			Args []string `xml:"array>string"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal(renderPlist(p), &plist); err != nil {
		t.Fatal(err)
	}
	want := []string{filepath.Join(p.Release, "garm"), "--config", p.GARMConfig}
	if !reflect.DeepEqual(plist.Dict.Args, want) {
		t.Fatalf("invalid launchd argv: %q", plist.Dict.Args)
	}
}

func TestCLIRecoveryClassificationRestrictsControllerAndSecrets(t *testing.T) {
	for _, args := range [][]string{
		{"profile", "login"}, {"--format", "table", "profile", "add"}, {"profile", "create", "--url=" + controllerURL, "--name=" + profileName}, {"profile", "switch", profileName},
	} {
		class, err := classifyCLI(args)
		if err != nil || !class.Recovery {
			t.Fatalf("recovery blocked %q: %v", args, err)
		}
	}
	for _, args := range [][]string{
		{"profile", "add", "--url", "https://foreign.example"}, {"profile", "login", "--password", "secret"},
		{"profile", "add", "-ahttps://foreign.example"}, {"profile", "add", "--name", "foreign"},
		{"profile", "switch", "foreign"}, {"--debug", "profile", "login"}, {"profile", "list", "--format", "json"},
		{"init"}, {"profile", "add", "--url", controllerURL, "--url", "https://foreign.example"},
	} {
		if _, err := classifyCLI(args); err == nil {
			t.Fatalf("unsafe recovery accepted: %q", args)
		}
	}
	class, err := classifyCLI([]string{"--format=json", "controller", "show"})
	if err != nil || class.Recovery {
		t.Fatalf("ordinary request bypasses authenticated mode: %+v %v", class, err)
	}
}

func TestProfileAllowsExpiredTokenRecoveryButRejectsForeignURL(t *testing.T) {
	p, err := pathsFor(t.TempDir(), "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareCLIHome(p); err != nil {
		t.Fatal(err)
	}
	writeProfile := func(url, token string) {
		t.Helper()
		data := fmt.Sprintf("active_manager = %q\n[[manager]]\nname = %q\nbase_url = %q\nbearer_token = %q\n", profileName, profileName, url, token)
		if err := os.WriteFile(profilePath(p), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeProfile(controllerURL, "")
	if err := validateProfile(p, false); err != nil {
		t.Fatal("recovery requires a token", err)
	}
	if err := validateProfile(p, true); err == nil {
		t.Fatal("ordinary request allowed without authentication")
	}
	writeProfile("https://foreign.example", "expired-token")
	if err := validateProfile(p, false); err == nil {
		t.Fatal("recovery may target an unrelated controller")
	}
}

func TestDoctorHardFailurePrecedesMissingTemplate(t *testing.T) {
	for _, test := range []struct {
		failures     []error
		images, code int
	}{
		{[]error{errors.New("TLS failed")}, 0, 1}, {nil, 0, 2}, {nil, 1, 0},
	} {
		err := doctorResult(test.failures, test.images)
		if test.code == 0 {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		var exit interface{ ExitCode() int }
		if !errors.As(err, &exit) || exit.ExitCode() != test.code {
			t.Fatalf("doctor exit mismatch: %v", err)
		}
	}
}

func TestOrbStackMinimumVersion(t *testing.T) {
	for version, want := range map[string]bool{"Version: 2.2.2 (2020200)": false, "Version: 2.2.3 (2020300)": true, "Version: 2.3.0": true, "Version: 3.0.0": true, "unknown": false} {
		if got := compatibleOrbVersion(version); got != want {
			t.Fatalf("%s: %v", version, got)
		}
	}
}
