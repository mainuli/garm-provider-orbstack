package templates

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	assets "github.com/mainuli/garm-provider-orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
)

// recipeFS is rooted at the embedded recipe directory. An explicit operator
// override is useful when intentionally building a new recipe; its exact bytes
// are still hashed into the resulting immutable manifest.
var recipeFS fs.FS = embeddedRecipes()

func embeddedRecipes() fs.FS {
	if dir := os.Getenv("GARM_ORBSTACK_RECIPES_DIR"); dir != "" {
		return os.DirFS(dir)
	}
	root, err := fs.Sub(assets.Files, "images/ubuntu-24.04")
	if err != nil {
		panic(err)
	}
	return root
}

// The full variant applies the official actions/runner-images Ubuntu 24.04
// arm64 toolset at a pinned tag+commit; both are verified inside the recipe
// (the clone's HEAD must equal the commit) and hashed into RecipeSHA256 via
// the "runner-images-pin" entry, so bumping the pin always changes the hash.
const (
	runnerImagesTag    = "ubuntu24/20260920.314"
	runnerImagesCommit = "e75633902841aa5479c759492b73409e6d317f12"
)

type BuildOptions struct {
	Arch          string
	RunnerVersion string
	RunnerSHA256  string
	// Variant selects the software scope: "minimal" (default) or "full"
	// (official runner-images toolset).
	Variant string
}

func (o BuildOptions) Validate() error {
	if o.Arch != "arm64" && o.Arch != "amd64" {
		return errors.New("template architecture must be arm64 or amd64")
	}
	if !runnerVersionPattern.MatchString(o.RunnerVersion) {
		return errors.New("runner-version must be an explicit numeric X.Y.Z release")
	}
	if !ValidSHA256(o.RunnerSHA256) {
		return errors.New("runner-sha256 must be a verified 64-character SHA-256")
	}
	if o.Variant == "" {
		o.Variant = "minimal"
	}
	if o.Variant != "minimal" && o.Variant != "full" {
		return errors.New("variant must be minimal or full")
	}
	if o.Variant == "full" && o.Arch != "arm64" {
		return errors.New("the full runner-images toolset variant is currently pinned to arm64")
	}
	return nil
}

// tailWriter keeps the last limit bytes written to it, so provisioning
// errors can carry a guest stderr tail instead of a bare exit status.
type tailWriter struct {
	limit int
	buf   []byte
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailWriter) summary() string {
	s := strings.TrimSpace(string(t.buf))
	if s == "" {
		return "no guest stderr"
	}
	if len(s) > 512 {
		s = "..." + s[len(s)-512:]
	}
	return "guest stderr tail: " + s
}

func recipe() ([]byte, []byte, []byte, string, error) {
	prepare, err := fs.ReadFile(recipeFS, "prepare.sh")
	if err != nil {
		return nil, nil, nil, "", err
	}
	seal, err := fs.ReadFile(recipeFS, "seal.sh")
	if err != nil {
		return nil, nil, nil, "", err
	}
	toolset, err := fs.ReadFile(recipeFS, "install-github-toolset.sh")
	if err != nil {
		return nil, nil, nil, "", err
	}
	hash := sha256.New()
	for _, entry := range []struct {
		name string
		data []byte
	}{{"prepare.sh", prepare}, {"seal.sh", seal}, {"install-github-toolset.sh", toolset}, {"runner-images-pin", []byte(runnerImagesTag + "\x00" + runnerImagesCommit)}} {
		fmt.Fprintf(hash, "%s\x00%d\x00", entry.name, len(entry.data))
		hash.Write(entry.data)
	}
	return prepare, seal, toolset, hex.EncodeToString(hash.Sum(nil)), nil
}

// Build creates a new credential-free machine. No existing template is updated
// or deleted. Once sealed, every uncertain commit retains the stopped machine
// and identifies it for operator inspection instead of risking a registered image.
func Build(ctx context.Context, configPath string, options BuildOptions) (Manifest, error) {
	if err := options.Validate(); err != nil {
		return Manifest{}, err
	}
	host, err := config.LoadHost(configPath)
	if err != nil {
		return Manifest{}, err
	}
	prepare, seal, toolset, recipeHash, err := recipe()
	if err != nil {
		return Manifest{}, err
	}
	orb := orbstack.New(host.OrbctlPath)
	version, err := orb.Version(ctx)
	if err != nil {
		return Manifest{}, err
	}
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return Manifest{}, err
	}
	variant := options.Variant
	if variant == "" {
		variant = "minimal"
	}
	imageID := fmt.Sprintf("ubuntu-24.04-%s-%s-%s-%s", options.Arch, variant, options.RunnerVersion, hex.EncodeToString(nonce[:]))
	machineName := "garm-template-" + options.Arch + "-" + hex.EncodeToString(nonce[:])
	if err := orb.Create(ctx, orbstack.CreateOptions{Name: machineName, Distro: "ubuntu", Version: "24.04", Arch: options.Arch, User: "runner", Isolated: true}); err != nil {
		return Manifest{}, fmt.Errorf("template creation outcome uncertain; inspect machine name %s: %w", machineName, err)
	}
	info, err := orb.Info(ctx, machineName)
	if err != nil {
		return Manifest{}, fmt.Errorf("template created but identity unrecorded; inspect %s: %w", machineName, err)
	}
	machineID := info.Record.ID
	if machineID == "" || info.Record.Name != machineName {
		return Manifest{}, errors.New("template identity is uncertain; inspect the newly created machine")
	}
	// Record the version label OrbStack assigns ("noble" for Ubuntu 24.04
	// on 2.2.3); validation compares clones against this observed value.
	osVersion := info.Record.Image.Version
	fail := func(cause error) (Manifest, error) {
		// A build failure is inspectable and never starts an unsealed machine
		// again automatically. Stop only the ID returned by our create.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), host.OperationTimeout)
		defer cancel()
		stopErr := orb.Stop(stopCtx, machineID)
		return Manifest{}, fmt.Errorf("template %s retained for inspection (name %s): %w", machineID, machineName, errors.Join(cause, stopErr))
	}
	// A template machine is a build context, not a runner: the default
	// flavor's runner-sized resource caps (2 CPU / 4 GiB) OOM-kill a full
	// toolset install (multi-GB toolchains, browsers, Swift extracting ~4 GiB
	// into /tmp under a 4 GiB cgroup), which stops the guest mid-build. Build
	// templates with no resource caps (zero = unset: the whole-VM defaults
	// apply); every clone gets the pool flavor caps applied by the provider
	// before its first boot, so nothing about the template's own limits
	// leaks into runners.
	if err := orb.ApplySettings(ctx, machineName, orbstack.MachineSettings{Isolated: true}); err != nil {
		return fail(err)
	}
	run := func(argv []string, input io.Reader, output io.Writer) error {
		errTail := &tailWriter{limit: 4096}
		if err := orb.Run(ctx, orbstack.RunOptions{Machine: machineID, User: "root", Command: argv, Stdin: input, Stdout: output, Stderr: errTail}); err != nil {
			return fmt.Errorf("template provisioning command failed (%s): %w", errTail.summary(), err)
		}
		return nil
	}
	if err := run([]string{"install", "-d", "-m", "0700", "/var/tmp/garm-template-build"}, nil, io.Discard); err != nil {
		return fail(err)
	}
	for _, script := range []struct {
		path string
		data []byte
	}{{"/var/tmp/garm-template-build/prepare.sh", prepare}, {"/var/tmp/garm-template-build/seal.sh", seal}, {"/var/tmp/garm-template-build/install-github-toolset.sh", toolset}} {
		if err := run([]string{"tee", script.path}, bytes.NewReader(script.data), io.Discard); err != nil {
			return fail(err)
		}
	}
	arch := options.Arch
	if arch == "amd64" {
		arch = "x64"
	}
	filename := fmt.Sprintf("actions-runner-linux-%s-%s.tar.gz", arch, options.RunnerVersion)
	if err := run([]string{"sh", "/var/tmp/garm-template-build/prepare.sh", options.Arch, options.RunnerVersion, strings.ToLower(options.RunnerSHA256), filename}, nil, io.Discard); err != nil {
		return fail(err)
	}
	if variant == "full" {
		// Hours-long official toolset application; runs on the ambient
		// build context (the CLI's signal context), not the controller
		// operation timeout.
		if err := run([]string{"bash", "/var/tmp/garm-template-build/install-github-toolset.sh", runnerImagesTag, runnerImagesCommit}, nil, io.Discard); err != nil {
			return fail(err)
		}
	}
	if err := run([]string{"sh", "/var/tmp/garm-template-build/seal.sh"}, nil, io.Discard); err != nil {
		return fail(err)
	}
	var packageOutput bytes.Buffer
	if err := run([]string{"dpkg-query", "-W", "-f=${binary:Package}\t${Version}\n"}, nil, &packageOutput); err != nil {
		return fail(err)
	}
	packages, err := parsePackages(packageOutput.String())
	if err != nil {
		return fail(err)
	}
	manifest := Manifest{SchemaVersion: SchemaVersion, ImageID: imageID, MachineID: machineID,
		OSVersion: osVersion, Variant: variant, RecipeSHA256: recipeHash, Arch: options.Arch, RunnerFilename: filename,
		RunnerSHA256: strings.ToLower(options.RunnerSHA256), OrbStackVersion: version, Packages: packages}
	metadata, err := json.Marshal(manifest)
	if err != nil {
		return fail(err)
	}
	if err := run([]string{"install", "-d", "-m", "0755", "/etc/garm-template"}, nil, io.Discard); err != nil {
		return fail(err)
	}
	if err := run([]string{"tee", "/etc/garm-template/manifest.json"}, bytes.NewReader(metadata), io.Discard); err != nil {
		return fail(err)
	}
	if err := run([]string{"chmod", "0644", "/etc/garm-template/manifest.json"}, nil, io.Discard); err != nil {
		return fail(err)
	}
	if err := run([]string{"rm", "-rf", "/var/tmp/garm-template-build"}, nil, io.Discard); err != nil {
		return fail(err)
	}
	if err := orb.Stop(ctx, machineID); err != nil {
		return fail(err)
	}
	stopped, err := orb.Info(ctx, machineID)
	if err != nil {
		return fail(err)
	}
	if err := ValidateMachine(manifest, stopped.Record); err != nil {
		return fail(err)
	}
	manifestDir := filepath.Join(host.StateDir, "manifests")
	if err := os.MkdirAll(manifestDir, 0700); err != nil {
		return manifest, fmt.Errorf("sealed template %s retained; creating manifest directory: %w", machineID, err)
	}
	manifestPath := filepath.Join(manifestDir, imageID+".json")
	if err := WriteManifest(manifestPath, manifest); err != nil {
		return manifest, fmt.Errorf("sealed template %s retained; manifest commit at %s failed: %w", machineID, manifestPath, err)
	}
	if err := RegisterImage(configPath, host, manifest, manifestPath); err != nil {
		return manifest, fmt.Errorf("sealed template %s retained with manifest %s; registration uncertain: %w", machineID, manifestPath, err)
	}
	return manifest, nil
}

func parsePackages(output string) (map[string]string, error) {
	packages := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(output, "\n"), "\n") {
		name, version, ok := strings.Cut(line, "\t")
		if !ok || name == "" || version == "" || strings.ContainsAny(version, "\t\r") {
			return nil, errors.New("invalid installed package inventory")
		}
		if _, exists := packages[name]; exists {
			return nil, errors.New("duplicate installed package entry")
		}
		packages[name] = version
	}
	return packages, nil
}

// RegisterImage is the sole template registration path. It shares the host
// writer's lock with installation and preserves every other image and setting.
func RegisterImage(path string, original config.Host, manifest Manifest, manifestPath string) error {
	if err := validateManifest(manifest); err != nil {
		return err
	}
	image := config.Image{MachineID: manifest.MachineID, ManifestPath: manifestPath, Arch: manifest.Arch}
	return config.UpdateHost(path, func(host *config.Host) error {
		if host.ControllerID != original.ControllerID || host.StateDir != original.StateDir || host.OrbctlPath != original.OrbctlPath {
			return errors.New("host identity or paths changed during template build")
		}
		if previous, exists := host.Images[manifest.ImageID]; exists {
			if previous != image {
				return errors.New("immutable image ID is already registered differently")
			}
			return nil
		}
		for _, previous := range host.Images {
			if previous.MachineID == manifest.MachineID {
				return errors.New("template machine already registered under another image ID")
			}
		}
		host.Images[manifest.ImageID] = image
		return nil
	})
}

// List validates registered manifests against a successful complete inventory.
func List(ctx context.Context, host config.Host) ([]Manifest, error) {
	machines, err := orbstack.New(host.OrbctlPath).List(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]orbstack.Machine, len(machines))
	for _, machine := range machines {
		if machine.ID == "" {
			return nil, errors.New("invalid OrbStack inventory")
		}
		if _, duplicate := byID[machine.ID]; duplicate {
			return nil, errors.New("duplicate machine ID in OrbStack inventory")
		}
		byID[machine.ID] = machine
	}
	ids := make([]string, 0, len(host.Images))
	for id := range host.Images {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result := make([]Manifest, 0, len(ids))
	for _, id := range ids {
		image := host.Images[id]
		manifest, err := LoadManifest(image.ManifestPath)
		if err != nil {
			return nil, err
		}
		if manifest.ImageID != id || manifest.MachineID != image.MachineID || manifest.Arch != image.Arch {
			return nil, errors.New("registered image differs from its manifest")
		}
		if err := ValidateMachine(manifest, byID[image.MachineID]); err != nil {
			return nil, fmt.Errorf("template %s: %w", id, err)
		}
		result = append(result, manifest)
	}
	return result, nil
}

// Remove deregisters a template image and deletes its machine. It refuses
// while any registry record still references the image (drain the pool
// first), never deletes a machine outside the registered mapping, and
// routes the machine deletion through the ID-addressed provider path.
func Remove(ctx context.Context, configPath, imageID string) error {
	if imageID == "" {
		return errors.New("image ID is required")
	}
	host, err := config.LoadHost(configPath)
	if err != nil {
		return err
	}
	image, registered := host.Images[imageID]
	if !registered {
		return fmt.Errorf("image %s is not registered", imageID)
	}
	// The registration is about to authorize an irreversible delete; it must
	// agree with its manifest, and no other image may share the machine or
	// manifest (the shape hand edits leave behind).
	manifest, err := LoadManifest(image.ManifestPath)
	if err != nil {
		return fmt.Errorf("image %s: %w", imageID, err)
	}
	if manifest.ImageID != imageID || manifest.MachineID != image.MachineID || manifest.Arch != image.Arch {
		return fmt.Errorf("image %s: registration differs from its manifest; refusing to delete machine %s", imageID, image.MachineID)
	}
	for id, other := range host.Images {
		if id != imageID && (other.MachineID == image.MachineID || other.ManifestPath == image.ManifestPath) {
			return fmt.Errorf("image %s shares its machine or manifest with registered image %s; refusing", imageID, id)
		}
	}
	// Serialize against in-flight clones: the provider holds this lock from
	// template validation through orbctl clone, so a clone that started
	// earlier makes the drain check below see its record and refuse, and a
	// later clone blocks here, then fails before marking its intent.
	reg, err := state.Open(ctx, host.StateDir, host.ControllerID)
	if err != nil {
		return err
	}
	templateLock, err := reg.LockTemplate(ctx, image.MachineID)
	if err != nil {
		return err
	}
	defer templateLock.Close()
	records, err := reg.Snapshot(ctx)
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.ImageID == imageID {
			return fmt.Errorf("runner %s still uses image %s; drain the pool before removing the template (stale machine-less reservations: garm-orbstack recover; repoint or delete GARM scale sets naming this image)", record.RunnerName, imageID)
		}
	}
	orb := orbstack.New(host.OrbctlPath)
	if err := orb.Delete(ctx, image.MachineID); err != nil {
		return fmt.Errorf("deleting template machine (image stays registered): %w", err)
	}
	if err := config.UpdateHost(configPath, func(h *config.Host) error {
		if _, ok := h.Images[imageID]; !ok {
			return fmt.Errorf("image %s disappeared concurrently", imageID)
		}
		delete(h.Images, imageID)
		return nil
	}); err != nil {
		return fmt.Errorf("machine already deleted; re-run template remove to converge (image stays registered until then): %w", err)
	}
	if err := os.Remove(image.ManifestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("template deregistered but manifest %s remains: %w", image.ManifestPath, err)
	}
	return nil
}
