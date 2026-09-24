package install

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
	"golang.org/x/sys/unix"
)

type Options struct {
	ConfigPath           string
	FromDir              string
	ConfirmVersionChange bool
}

type installationRecord struct {
	Version         string `json:"version"`
	PreviousVersion string `json:"previous_version,omitempty"`
	ControllerID    string `json:"controller_id,omitempty"`
	HostConfig      string `json:"host_config"`
	StateDir        string `json:"state_dir"`
	Backup          string `json:"backup,omitempty"`
	// ManagedFiles maps each managed file path to the SHA-256 of the exact
	// content this installation wrote, so a later release accepts (and
	// replaces) the prior release's files even after render templates drift.
	ManagedFiles map[string]string `json:"managed_files,omitempty"`
}

func loadInstallation(p Paths) (installationRecord, error) {
	var record installationRecord
	data, err := os.ReadFile(p.Record)
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return record, err
	}
	if !validVersion(record.Version) || record.HostConfig != p.HostConfig || record.StateDir != p.State || (record.PreviousVersion != "" && !validVersion(record.PreviousVersion)) {
		return record, errors.New("installation record does not match managed paths/version")
	}
	return record, nil
}

func saveInstallation(p Paths, record installationRecord) error {
	data, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return atomicFile(p.Record, append(data, '\n'), 0o600)
}

func lockInstallation(p Paths) (*os.File, error) {
	if err := privateDir(p.ConfigDir); err != nil {
		return nil, err
	}
	path := filepath.Join(p.ConfigDir, "installation.lock")
	if err := rejectSymlinks(p.Home, path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("another install, doctor, or uninstall operation holds the installation lock")
	}
	return f, nil
}

func validateManagedLink(p Paths, versions ...string) error {
	target, err := os.Readlink(p.Link)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refusing unmanaged binary at %s: %w", p.Link, err)
	}
	for _, version := range versions {
		if version != "" && target == filepath.Join(p.Root, "releases", version, "garm-orbstack") {
			return nil
		}
	}
	return fmt.Errorf("refusing unmanaged symlink %s", p.Link)
}

func ensureLink(p Paths, versions ...string) error {
	if err := validateManagedLink(p, versions...); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p.Link), 0o700); err != nil {
		return err
	}
	target := filepath.Join(p.Release, "garm-orbstack")
	if existing, err := os.Readlink(p.Link); err == nil && existing == target {
		return nil
	}
	suffix, err := randomHex(8)
	if err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(p.Link), ".garm-orbstack-"+suffix)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Rename(tmp, p.Link); err != nil {
		return err
	}
	return syncDir(filepath.Dir(p.Link))
}

func placeRelease(p Paths, release verifiedRelease) error {
	if _, err := os.Lstat(p.Release); err == nil {
		old, err := verifyRelease(p.Release, release.Lock.Version, runtime.GOARCH, true)
		if err != nil {
			return fmt.Errorf("refusing differing/incomplete installed release: %w", err)
		}
		for rel, source := range release.Files {
			a, err := fileDigest(source)
			if err != nil {
				return err
			}
			b, err := fileDigest(old.Files[rel])
			if err != nil {
				return err
			}
			if a != b {
				return fmt.Errorf("different file for the same release version: %s", rel)
			}
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	parent := filepath.Dir(p.Release)
	if err := privateDir(parent); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".release-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := os.Mkdir(filepath.Join(tmp, "providers.d"), 0o700); err != nil {
		return err
	}
	for rel, source := range release.Files {
		mode := os.FileMode(0o755)
		if rel == "release-lock.json" || rel == "SHA256SUMS" {
			mode = 0o600
		}
		if err := copyNewFile(source, filepath.Join(tmp, rel), mode); err != nil {
			return err
		}
	}
	// Re-hash the copied bytes, closing a staging mutation window before publish.
	if _, err := verifyRelease(tmp, release.Lock.Version, runtime.GOARCH, true); err != nil {
		return err
	}
	if err := os.Rename(tmp, p.Release); err != nil {
		return err
	}
	return syncDir(parent)
}

func contentDigest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// acceptableManaged reports whether existing content may be replaced by us.
func acceptableManaged(existing, wanted []byte, allowed [][]byte, recordedDigest string) bool {
	if bytes.Equal(existing, wanted) {
		return true
	}
	for _, candidate := range allowed {
		if bytes.Equal(existing, candidate) {
			return true
		}
	}
	return recordedDigest != "" && contentDigest(existing) == recordedDigest
}

// managedWrite writes wanted when the existing content is ours (identical,
// an allowed previous render, or matching the digest recorded at write
// time). It returns whether the content changed on disk.
func managedWrite(record *installationRecord, path string, wanted []byte, allowed ...[]byte) (bool, error) {
	recordedDigest := record.ManagedFiles[path]
	old, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !acceptableManaged(old, wanted, allowed, recordedDigest) {
			return false, fmt.Errorf("refusing to overwrite changed/unmanaged file %s", path)
		}
		if bytes.Equal(old, wanted) {
			// Adopt identical pre-existing content: a resumed install may
			// have written this file before the record was first saved, and
			// without a recorded digest every future version change would
			// refuse to replace it.
			if record.ManagedFiles == nil {
				record.ManagedFiles = map[string]string{}
			}
			record.ManagedFiles[path] = contentDigest(wanted)
			return false, nil
		}
	case errors.Is(err, os.ErrNotExist):
	default:
		return false, err
	}
	if err := atomicFile(path, wanted, 0o600); err != nil {
		return false, err
	}
	if record.ManagedFiles == nil {
		record.ManagedFiles = map[string]string{}
	}
	record.ManagedFiles[path] = contentDigest(wanted)
	return true, nil
}

// checkManagedFiles verifies, WITHOUT writing, that every managed file is
// acceptable for our replacement. The version-change flow runs this BEFORE
// stopping the controller so a refusal never leaves it stopped.
func checkManagedFiles(record *installationRecord, specs []struct {
	path    string
	wanted  []byte
	allowed [][]byte
}) error {
	for _, spec := range specs {
		old, err := os.ReadFile(spec.path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if !acceptableManaged(old, spec.wanted, spec.allowed, record.ManagedFiles[spec.path]) {
			return fmt.Errorf("managed file %s was changed by something else; refusing version change", spec.path)
		}
	}
	return nil
}

func backupStopped(p Paths, version string) (string, error) {
	parent := filepath.Join(p.Root, "backups")
	if err := privateDir(parent); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(parent, ".snapshot-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	for name, source := range map[string]string{"state": p.State, "config": p.ConfigDir, "secrets": p.Secrets} {
		err := filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			rel, err := filepath.Rel(source, path)
			if err != nil {
				return err
			}
			dest := filepath.Join(tmp, name, rel)
			if entry.IsDir() {
				return os.MkdirAll(dest, 0o700)
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("refusing non-regular backup entry %s", path)
			}
			return copyNewFile(path, dest, 0o600)
		})
		if err != nil {
			return "", err
		}
	}
	final := filepath.Join(parent, time.Now().UTC().Format("20060102T150405.000000000Z")+"-"+version)
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return final, syncDir(parent)
}

// Install verifies the entire candidate before resolving the owner or touching
// managed paths, then resumes a controller-bound installation transaction.
func Install(ctx context.Context, opts Options) error {
	if err := ValidateReleaseMetadata(); err != nil {
		return err
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	exe, err = filepath.EvalSymlinks(exe)
	if err != nil {
		return err
	}
	dir, installed := opts.FromDir, false
	if dir == "" {
		dir, installed = filepath.Dir(exe), true
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	release, err := verifyRelease(dir, releaseinfo.Version, runtime.GOARCH, installed)
	if err != nil {
		return fmt.Errorf("release staging rejected before installation: %w", err)
	}
	if err := verifyDigest(exe, release.Sums[executableAssets(releaseinfo.Version, runtime.GOARCH)["garm-orbstack"]]); err != nil {
		return err
	}
	for rel, path := range release.Files {
		if rel != "release-lock.json" && rel != "SHA256SUMS" {
			if !installed {
				if err := os.Chmod(path, 0o755); err != nil {
					return err
				}
			}
		}
	}
	if err := verifyProvenance(ctx, release); err != nil {
		return err
	}
	orbctl, launchctl, err := preflight(ctx)
	if err != nil {
		return err
	}
	// A successful inventory confirms the backend is running, not merely that
	// the CLI executable is present. Never manipulate unrelated machines.
	inventoryCtx, inventoryCancel := context.WithTimeout(ctx, 30*time.Second)
	_, err = orbstack.New(orbctl).List(inventoryCtx)
	inventoryCancel()
	if err != nil {
		return fmt.Errorf("OrbStack inventory preflight: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	p, err := pathsFor(home, opts.ConfigPath, releaseinfo.Version)
	if err != nil {
		return err
	}
	if err := p.check(); err != nil {
		return err
	}
	record, err := loadInstallation(p)
	fresh := errors.Is(err, os.ErrNotExist)
	if err != nil && !fresh {
		return err
	}
	if fresh {
		_, loaded, err := launchStatus(ctx, launchctl)
		if err != nil {
			return err
		}
		if loaded {
			return errors.New("refusing pre-existing unmanaged dev.orbstack.garm LaunchAgent")
		}
		for _, path := range []string{p.HostConfig, p.GARMConfig, p.Secrets, p.State, p.Plist, p.Link, p.Release} {
			if _, err := os.Lstat(path); err == nil {
				return fmt.Errorf("refusing pre-existing unmanaged installation path %s", path)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		record = installationRecord{Version: releaseinfo.Version, HostConfig: p.HostConfig, StateDir: p.State}
	}
	if record.Version != releaseinfo.Version && !opts.ConfirmVersionChange {
		return errors.New("different installed release: stop/drain runners and rerun with --confirm-version-change for a stopped-controller DB/config/secrets/registry backup; binary-only rollback is unsafe")
	}
	if err := validateManagedLink(p, record.Version, record.PreviousVersion); err != nil {
		return err
	}
	lock, err := lockInstallation(p)
	if err != nil {
		return err
	}
	defer lock.Close()
	// Recheck after locking rather than trusting a stale preflight snapshot.
	if !fresh {
		current, err := loadInstallation(p)
		if err != nil {
			return err
		}
		if current.Version != record.Version || current.ControllerID != record.ControllerID || current.Backup != record.Backup || current.PreviousVersion != record.PreviousVersion || !maps.Equal(current.ManagedFiles, record.ManagedFiles) {
			return errors.New("installation changed during preflight; rerun")
		}
	} else {
		if _, err := os.Lstat(p.Record); err == nil {
			return errors.New("another installation completed during preflight; rerun")
		}
		if err := saveInstallation(p, record); err != nil {
			return err
		}
	}
	if record.Version != releaseinfo.Version {
		host, err := config.LoadHost(p.HostConfig)
		if err != nil {
			return err
		}
		records, err := state.Snapshot(ctx, p.State, host.ControllerID)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			return errors.New("drain and remove managed runners before a version change")
		}
		// Refuse the upgrade BEFORE stopping the controller if any managed
		// file is no longer recognizably ours.
		// Read-only: the pre-check must never generate secrets. A missing
		// secrets directory with an existing database is a rotation refusal
		// that the later guards enforce; generating here would bypass them.
		preSecrets, preSecretsErr := loadSecrets(p)
		if preSecretsErr != nil {
			return preSecretsErr
		}
		_, preHostErr := config.LoadHost(p.HostConfig)
		preHasHost := preHostErr == nil
		// The currently INSTALLED release wrote the on-disk files; accept
		// its renders (and the one before it, mid-upgrade resumes) even
		// after our templates drift. record.Version is the installed one
		// here because the swap to the new version happens only below.
		installed, _ := pathsFor(p.Home, p.HostConfig, record.Version)
		prevConfig, prevPlist := renderGARMConfig(installed, preSecrets, true), renderPlist(installed)
		var extraConfig, extraPlist []byte
		if record.PreviousVersion != "" && record.PreviousVersion != record.Version {
			older, _ := pathsFor(p.Home, p.HostConfig, record.PreviousVersion)
			extraConfig, extraPlist = renderGARMConfig(older, preSecrets, true), renderPlist(older)
		}
		if err := checkManagedFiles(&record, []struct {
			path    string
			wanted  []byte
			allowed [][]byte
		}{
			{p.GARMConfig, renderGARMConfig(p, preSecrets, preHasHost), [][]byte{renderGARMConfig(p, preSecrets, false), prevConfig, extraConfig}},
			{p.Plist, renderPlist(p), [][]byte{prevPlist, extraPlist}},
		}); err != nil {
			return err
		}
		if err := stopService(ctx, launchctl); err != nil {
			return err
		}
		records, err = state.Snapshot(ctx, p.State, host.ControllerID)
		if err != nil {
			return err
		}
		if len(records) != 0 {
			return errors.New("runner records appeared while stopping; controller stopped without upgrading")
		}
		backup, err := backupStopped(p, record.Version)
		if err != nil {
			return fmt.Errorf("controller stopped; backup failed without upgrading: %w", err)
		}
		record.PreviousVersion, record.Version, record.Backup = record.Version, releaseinfo.Version, backup
		if err := saveInstallation(p, record); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Stopped-controller backup:", backup)
	}
	if err := placeRelease(p, release); err != nil {
		return err
	}
	if record.ControllerID != "" {
		if _, err := os.Stat(filepath.Join(p.State, "garm.db")); err != nil {
			return fmt.Errorf("preserved controller database is missing; refusing reinitialization: %w", err)
		}
	}
	if _, err := os.Stat(filepath.Join(p.State, "garm.db")); err == nil {
		if _, err := os.Stat(p.Secrets); err != nil {
			return fmt.Errorf("database exists without preserved secrets; refusing secret rotation: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	secrets, err := ensureSecrets(p)
	if err != nil {
		return err
	}
	for _, path := range []string{p.State, filepath.Dir(p.GARMConfig)} {
		if err := privateDir(path); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(p.Plist), 0o700); err != nil {
		return err
	}
	var host config.Host
	host, hostErr := config.LoadHost(p.HostConfig)
	if hostErr != nil && !errors.Is(hostErr, os.ErrNotExist) {
		return hostErr
	}
	hasHost := hostErr == nil
	if hasHost && (host.StateDir != p.State || host.OrbctlPath != orbctl || (record.ControllerID != "" && host.ControllerID != record.ControllerID)) {
		return errors.New("existing controller identity/OrbStack/state paths do not match installation")
	}
	if record.ControllerID != "" && !hasHost {
		return errors.New("preserved host.toml is missing; refusing controller rebinding")
	}
	if hasHost {
		if err := state.Initialize(ctx, p.State); err != nil {
			return err
		}
	}
	var oldConfig, oldPlist []byte
	if record.PreviousVersion != "" {
		previous, _ := pathsFor(p.Home, p.HostConfig, record.PreviousVersion)
		oldConfig, oldPlist = renderGARMConfig(previous, secrets, true), renderPlist(previous)
	}
	configChangedEarly, err := managedWrite(&record, p.GARMConfig, renderGARMConfig(p, secrets, hasHost), renderGARMConfig(p, secrets, false), oldConfig)
	if err != nil {
		return err
	}
	if _, err := managedWrite(&record, p.Plist, renderPlist(p), oldPlist); err != nil {
		return err
	}
	bootstrappedEarly, err := startService(ctx, launchctl, p, false)
	if err != nil {
		return err
	}
	status, err := waitController(ctx, p)
	if err != nil {
		return err
	}
	if status == 409 {
		if record.ControllerID != "" || hasHost {
			return errors.New("previously bound controller now reports initialization required; refusing reinitialization")
		}
		if err := cliInteractive(ctx, p, "init", "--name", profileName, "--url", controllerURL,
			"--callback-url", guestURL+"/api/v1/callbacks", "--metadata-url", guestURL+"/api/v1/metadata",
			"--agent-url", guestURL+"/agent", "--ca-bundle", p.CA); err != nil {
			return err
		}
	} else if err := validateProfile(p, true); err != nil {
		// Existing DB: add/login only, never init. A retained but expired token
		// uses the same recovery path below after the controller check fails.
		data, readErr := os.ReadFile(profilePath(p))
		if errors.Is(readErr, os.ErrNotExist) || (readErr == nil && len(bytes.TrimSpace(data)) == 0) {
			if err := cliInteractive(ctx, p, "profile", "add", "--name", profileName, "--url", controllerURL); err != nil {
				return err
			}
		} else if profileErr := validateProfile(p, false); profileErr == nil {
			if err := cliInteractive(ctx, p, "profile", "login"); err != nil {
				return err
			}
		} else {
			return err
		}
	}
	info, err := readController(ctx, p, record.ControllerID)
	if err != nil {
		if profileErr := validateProfile(p, false); profileErr != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "Authenticate to the preserved controller (no reinitialization).")
		if err := cliInteractive(ctx, p, "profile", "login"); err != nil {
			return err
		}
		info, err = readController(ctx, p, record.ControllerID)
		if err != nil {
			return err
		}
	}
	if hasHost && info.ID != host.ControllerID {
		return errors.New("controller UUID differs from preserved host.toml")
	}
	if err := validateControllerCA(p, info); err != nil {
		if record.ControllerID != "" || hasHost {
			return err
		}
		// init can save credentials before its controller update fails. Resume
		// only this unbound first installation, using the two distinct CA roles.
		if err := cliInteractive(ctx, p, "controller", "update", "--ca-bundle", p.CA, "--metadata-url", guestURL+"/api/v1/metadata", "--callback-url", guestURL+"/api/v1/callbacks", "--agent-url", guestURL+"/agent"); err != nil {
			return err
		}
		info, err = readController(ctx, p, info.ID)
		if err != nil {
			return err
		}
		if err := validateControllerCA(p, info); err != nil {
			return err
		}
	}
	if !hasHost {
		if err := config.UpdateHost(p.HostConfig, func(h *config.Host) error {
			if h.ControllerID != "" {
				return errors.New("host configuration appeared concurrently; refusing overwrite")
			}
			*h = config.Host{ControllerID: info.ID, OrbctlPath: orbctl, StateDir: p.State, MaxInstances: 2, OperationTimeout: 2 * time.Minute,
				Images: map[string]config.Image{}, Flavors: map[string]config.Flavor{"default": {CPUs: 2, MemoryMiB: 4096, DiskBytes: 68719476736}}}
			return nil
		}); err != nil {
			return err
		}
	}
	if err := state.Initialize(ctx, p.State); err != nil {
		return err
	}
	record.ControllerID = info.ID
	if err := saveInstallation(p, record); err != nil {
		return err
	}
	configChangedLate, err := managedWrite(&record, p.GARMConfig, renderGARMConfig(p, secrets, true), renderGARMConfig(p, secrets, false))
	if err != nil {
		return err
	}
	// An interrupted earlier run may have enabled the provider on disk but
	// died before restarting; only the combined change signal makes the
	// restart decision correct on resume.
	// A freshly bootstrapped controller has already loaded the early
	// config; restarting again would kill provider work it just started.
	needsRestart := configChangedLate || (configChangedEarly && !bootstrappedEarly)
	if err := saveInstallation(p, record); err != nil {
		return err
	}
	// Restarting (kickstart -k) kills the controller's process group,
	// including any in-flight provider clone holding its inherited lock. A
	// same-release repeat install with unchanged files must therefore NOT
	// restart; only a real content change (or a stopped service) does.
	if needsRestart {
		if _, err := startService(ctx, launchctl, p, true); err != nil {
			return err
		}
	} else if _, err := startService(ctx, launchctl, p, false); err != nil {
		return err
	}
	if _, err := waitController(ctx, p); err != nil {
		return err
	}
	if err := ensureProvider(ctx, launchctl, p); err != nil {
		return err
	}
	info, err = readController(ctx, p, record.ControllerID)
	if err != nil {
		return err
	}
	if err := validateControllerCA(p, info); err != nil {
		return err
	}
	if err := ensureLink(p, record.Version, record.PreviousVersion); err != nil {
		return err
	}
	record.PreviousVersion = ""
	if err := saveInstallation(p, record); err != nil {
		return err
	}
	printGuidance(p)
	return nil
}

func printGuidance(p Paths) {
	fmt.Fprintf(os.Stdout, "Installed %s. Controller: %s\nCA for explicit browser trust (not installed globally): %s\n", p.Link, controllerURL, p.CA)
	fmt.Fprintln(os.Stdout, "No GitHub credentials or scale sets were created. Build a versioned template, then import a scoped GitHub App for trusted repositories using garm-orbstack garm-cli github credentials add --help and --private-key-path.")
	fmt.Fprintln(os.Stdout, "Create a GitHub scale set named orbstack-linux-arm64 with --provider-name orbstack --image IMAGE_ID --flavor default --runner-install-template github_linux --os-type linux --os-arch arm64 --min-idle-runners=0 --max-runners=2 --enabled (choose its repository/organization with garm-orbstack garm-cli scaleset create --help).")
	fmt.Fprintln(os.Stdout, "Verify the scale set resolves template_name=github_linux and a nonzero template_id; target runs-on: orbstack-linux-arm64. Run doctor before enabling jobs. OrbStack shares a Linux kernel: this is not a hostile-tenant boundary.")
}

func sortedImageIDs(images map[string]config.Image) []string {
	ids := make([]string, 0, len(images))
	for id := range images {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
