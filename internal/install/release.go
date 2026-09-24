package install

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
)

const upstreamTag = "v0.2.1"
const upstreamCommit = "154638445c3949c1958b01812f69d9a1e4d82684"
const upstreamSource = "cloudbase/garm " + upstreamTag + " (" + upstreamCommit + ")"

var versionPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+$`)

func validVersion(v string) bool { return versionPattern.MatchString(v) }

// ValidateReleaseMetadata must precede even home-directory resolution. A dev
// binary cannot create an installation, even when handed plausible staged files.
func ValidateReleaseMetadata() error {
	if !validVersion(releaseinfo.Version) || releaseinfo.GARMSource != upstreamSource {
		return errors.New("not an installable release: missing or invalid version/GARM source metadata; download a checksummed versioned release")
	}
	return nil
}

type ReleaseLock struct {
	Version    string `json:"version"`
	GARMSource struct {
		Tag    string `json:"tag"`
		Commit string `json:"commit"`
	} `json:"garm_source"`
	GoVersion      string `json:"go_version"`
	CLIPatchSHA256 string `json:"cli_patch_sha256"`
}

type verifiedRelease struct {
	Lock      ReleaseLock
	Files     map[string]string // installed relative path -> verified source path
	Sums      map[string]string
	Directory string
}

func executableAssets(version, arch string) map[string]string {
	return map[string]string{
		"garm-orbstack":                      "garm-orbstack_" + version + "_darwin_" + arch,
		"providers.d/garm-provider-orbstack": "garm-provider-orbstack_" + version + "_darwin_" + arch,
		"garm":                               "garm_" + version + "_darwin_" + arch,
		"garm-cli":                           "garm-cli_" + version + "_darwin_" + arch,
	}
}

func parseSums(data []byte) (map[string]string, error) {
	sums := make(map[string]string)
	scan := bufio.NewScanner(strings.NewReader(string(data)))
	for scan.Scan() {
		line := scan.Text()
		if line == "" {
			continue
		}
		if len(line) < 67 || line[64] != ' ' || (line[65] != ' ' && line[65] != '*') {
			return nil, errors.New("malformed SHA256SUMS entry")
		}
		digest, name := line[:64], line[66:]
		if _, err := hex.DecodeString(digest); err != nil || name == "" || filepath.Base(name) != name || strings.ContainsAny(name, "\\\r\n\t ") {
			return nil, errors.New("invalid digest or asset filename in SHA256SUMS")
		}
		if _, exists := sums[name]; exists {
			return nil, fmt.Errorf("duplicate checksum entry for %s", name)
		}
		sums[name] = strings.ToLower(digest)
	}
	if err := scan.Err(); err != nil {
		return nil, err
	}
	return sums, nil
}

func fileDigest(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("release asset is not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func verifyDigest(path, expected string) error {
	if expected == "" {
		return fmt.Errorf("missing checksum for %s", filepath.Base(path))
	}
	got, err := fileDigest(path)
	if err != nil {
		return err
	}
	if got != expected {
		return fmt.Errorf("checksum mismatch for %s", filepath.Base(path))
	}
	return nil
}

// verifyRelease accepts either downloader staging names or installed names. It
// never writes managed state; all staged bytes are hashed before any execution.
func verifyRelease(dir, version, arch string, installed bool) (verifiedRelease, error) {
	var result verifiedRelease
	if !validVersion(version) || (arch != "arm64" && arch != "amd64") {
		return result, errors.New("unsupported release version or architecture")
	}
	if _, err := fileDigest(filepath.Join(dir, "SHA256SUMS")); err != nil {
		return result, err
	}
	data, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS"))
	if err != nil {
		return result, err
	}
	sums, err := parseSums(data)
	if err != nil {
		return result, err
	}
	files := make(map[string]string)
	allowed := map[string]bool{"SHA256SUMS": true, "release-lock.json": true, "install.sh": true}
	for rel, asset := range executableAssets(version, arch) {
		name := asset
		if installed {
			name = rel
		}
		if err := rejectSymlinks(dir, filepath.Join(dir, name)); err != nil {
			return result, err
		}
		path := filepath.Join(dir, name)
		if err := verifyDigest(path, sums[asset]); err != nil {
			return result, err
		}
		files[rel] = path
		allowed[name] = true
	}
	lockPath := filepath.Join(dir, "release-lock.json")
	if err := verifyDigest(lockPath, sums["release-lock.json"]); err != nil {
		return result, err
	}
	if !installed {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return result, err
		}
		for _, entry := range entries {
			if !allowed[entry.Name()] || entry.IsDir() {
				return result, fmt.Errorf("unexpected staging entry %s", entry.Name())
			}
			if entry.Name() != "SHA256SUMS" {
				if err := verifyDigest(filepath.Join(dir, entry.Name()), sums[entry.Name()]); err != nil {
					return result, err
				}
			}
		}
	} else {
		delete(allowed, "install.sh")
		if err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			if rel == "." || (rel == "providers.d" && entry.IsDir()) {
				return nil
			}
			if !allowed[rel] || !entry.Type().IsRegular() {
				return fmt.Errorf("unexpected installed release entry %s", rel)
			}
			return nil
		}); err != nil {
			return result, err
		}
	}
	data, err = os.ReadFile(lockPath)
	if err != nil {
		return result, err
	}
	var lock ReleaseLock
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&lock); err != nil {
		return result, fmt.Errorf("invalid release lock: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, errors.New("release lock must contain one JSON object")
	}
	patch, err := hex.DecodeString(lock.CLIPatchSHA256)
	if lock.Version != version || lock.GARMSource.Tag != upstreamTag || lock.GARMSource.Commit != upstreamCommit || !strings.HasPrefix(lock.GoVersion, "go1.26.") || err != nil || len(patch) != sha256.Size {
		return result, errors.New("release lock provenance does not match the pinned release contract")
	}
	files["release-lock.json"] = lockPath
	files["SHA256SUMS"] = filepath.Join(dir, "SHA256SUMS")
	return verifiedRelease{Lock: lock, Files: files, Sums: sums, Directory: dir}, nil
}

func cleanEnvironment(home, ca string) []string {
	// Do not inherit debugging/proxy/credential settings that could leak bearer
	// tokens or direct a managed CLI at an unrelated controller.
	env := []string{"HOME=" + home, "PATH=/usr/bin:/bin:/usr/sbin:/sbin", "LANG=en_US.UTF-8"}
	if term := os.Getenv("TERM"); term != "" {
		env = append(env, "TERM="+term)
	}
	if ca != "" {
		env = append(env, "GARM_CLI_CA_BUNDLE="+ca)
	}
	return env
}

func verifyProvenance(ctx context.Context, release verifiedRelease) error {
	home, err := os.MkdirTemp("", "garm-provenance-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(home)
	for _, check := range []struct {
		name     string
		args     []string
		expected string
	}{
		{"garm", []string{"--version"}, upstreamSource},
		{"garm-cli", []string{"version"}, "garm-cli: " + upstreamSource + "\ngarm server: v0.0.0-unknown"},
		{"garm-orbstack", []string{"version"}, release.Lock.Version},
	} {
		commandCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		cmd := exec.CommandContext(commandCtx, release.Files[check.name], check.args...)
		cmd.Env = cleanEnvironment(home, "")
		out, runErr := cmd.Output()
		cancel()
		if runErr != nil {
			return fmt.Errorf("staging provenance %s failed: %w", check.name, runErr)
		}
		if strings.TrimSpace(string(out)) != check.expected {
			return fmt.Errorf("%s provenance does not match release-lock.json", check.name)
		}
	}
	return nil
}

func currentRelease(ctx context.Context, p Paths) (verifiedRelease, error) {
	if err := ValidateReleaseMetadata(); err != nil {
		return verifiedRelease{}, err
	}
	r, err := verifyRelease(p.Release, releaseinfo.Version, runtime.GOARCH, true)
	if err != nil {
		return r, err
	}
	return r, verifyProvenance(ctx, r)
}
