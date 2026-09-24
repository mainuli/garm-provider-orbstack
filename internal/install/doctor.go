package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

type DiagnosticError struct {
	Code int
	Err  error
}

func (e *DiagnosticError) Error() string { return e.Err.Error() }
func (e *DiagnosticError) Unwrap() error { return e.Err }
func (e *DiagnosticError) ExitCode() int { return e.Code }

func doctorResult(failures []error, imageCount int) error {
	if len(failures) != 0 {
		return &DiagnosticError{Code: 1, Err: errors.Join(failures...)}
	}
	if imageCount == 0 {
		return &DiagnosticError{Code: 2, Err: errors.New("controller healthy but no runner template is registered; run template build; clone-dependent callback checks were skipped")}
	}
	return nil
}

func Doctor(ctx context.Context, configPath string) error {
	if err := ValidateReleaseMetadata(); err != nil {
		return &DiagnosticError{1, err}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return &DiagnosticError{1, err}
	}
	p, err := pathsFor(home, configPath, releaseinfo.Version)
	if err != nil {
		return &DiagnosticError{1, err}
	}
	if err := p.check(); err != nil {
		return &DiagnosticError{1, err}
	}
	lock, err := lockInstallation(p)
	if err != nil {
		return &DiagnosticError{1, err}
	}
	defer lock.Close()
	var failures []error
	check := func(name string, err error) {
		if err != nil {
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
			return
		}
		fmt.Fprintln(os.Stdout, "OK:", name)
	}
	record, recordErr := loadInstallation(p)
	if recordErr == nil && record.Version != releaseinfo.Version {
		recordErr = errors.New("running helper differs from installed version")
	}
	check("installation identity", recordErr)
	_, err = currentRelease(ctx, p)
	check("installed release checksums and staging-mode provenance", err)
	_, err = loadSecrets(p)
	check("installation TLS and preserved secrets", err)
	orbctl, launchctl, err := preflight(ctx)
	check("macOS/OrbStack/LaunchAgent prerequisites", err)
	if err == nil {
		status, loaded, err := launchStatus(ctx, launchctl)
		if err == nil && (!loaded || !strings.Contains(string(status), "state = running")) {
			err = errors.New("LaunchAgent is not loaded and running")
		}
		check("LaunchAgent running", err)
	}
	status, err := probeHTTP(ctx, p, "/api/v1/controller")
	if err == nil && status != http.StatusUnauthorized && status != http.StatusForbidden {
		err = fmt.Errorf("unexpected unauthenticated controller status %d", status)
	}
	check("loopback CA-validated controller HTTPS", err)
	host, hostErr := config.LoadHost(p.HostConfig)
	check("host configuration", hostErr)
	if hostErr != nil {
		return doctorResult(failures, 0)
	}
	if host.StateDir != p.State || (orbctl != "" && host.OrbctlPath != orbctl) || (recordErr == nil && record.ControllerID != host.ControllerID) {
		check("controller binding and paths", errors.New("managed controller ID or paths drifted"))
	}
	check("configured orbctl executable", checkExecutable(host.OrbctlPath))
	records, err := state.Snapshot(ctx, host.StateDir, host.ControllerID)
	check("ownership registry snapshot", err)
	if err == nil {
		for _, r := range records {
			if r.MachineID == "" {
				fmt.Fprintf(os.Stderr, "Reservation requires inspection: runner=%q machine=%q phase=%q; use recover, never adopt/delete by name automatically.\n", r.RunnerName, r.MachineName, r.Phase)
			}
		}
	}
	info, err := readController(ctx, p, host.ControllerID)
	check("authenticated isolated profile and controller UUID", err)
	if err == nil {
		check("stored runner CA bundle and callback/metadata URLs", validateControllerCA(p, info))
	}
	check("orbstack provider registration", checkProvider(ctx, p))
	client := orbstack.New(host.OrbctlPath)
	inventoryCtx, cancel := context.WithTimeout(ctx, host.OperationTimeout)
	machines, inventoryErr := client.List(inventoryCtx)
	cancel()
	check("complete OrbStack inventory", inventoryErr)
	var approved []orbstack.Machine
	for _, id := range sortedImageIDs(host.Images) {
		img := host.Images[id]
		manifest, err := templates.LoadManifest(img.ManifestPath)
		if err == nil && (manifest.ImageID != id || manifest.MachineID != img.MachineID || manifest.Arch != img.Arch) {
			err = errors.New("manifest image/machine/architecture differs from registration")
		}
		check("template manifest "+id, err)
		if err != nil || inventoryErr != nil {
			continue
		}
		var found *orbstack.Machine
		for i := range machines {
			if machines[i].ID == img.MachineID {
				found = &machines[i]
				break
			}
		}
		if found == nil {
			err = errors.New("registered template is absent from inventory")
		} else if found.State != orbstack.StateStopped || !found.Config.Isolated || found.Config.ForwardSSHAgent || found.Config.IsolateNetwork || machineArch(found.Image.Arch) != img.Arch {
			err = errors.New("template must be stopped, isolated, without agent forwarding, and match its registered architecture")
		}
		check("template machine "+id, err)
		if err == nil {
			approved = append(approved, *found)
		}
	}
	if len(failures) == 0 && len(approved) != 0 {
		for _, machine := range approved {
			check("isolated clone metadata HTTPS ("+machine.ID+")", probeClone(ctx, p, host, client, machine))
		}
	} else if len(host.Images) != 0 {
		fmt.Fprintln(os.Stderr, "Clone-dependent checks skipped because a required non-clone check failed.")
	}
	return doctorResult(failures, len(host.Images))
}

func machineArch(arch string) string {
	switch arch {
	case "aarch64", "arm64":
		return "arm64"
	case "x86_64", "amd64":
		return "amd64"
	default:
		return arch
	}
}

func probeClone(ctx context.Context, p Paths, host config.Host, client *orbstack.Client, source orbstack.Machine) (resultErr error) {
	ctx, cancel := context.WithTimeout(ctx, host.OperationTimeout)
	defer cancel()
	suffix, err := randomHex(12)
	if err != nil {
		return err
	}
	name := "garm-doctor-" + suffix
	if err := client.Clone(ctx, source.ID, name); err != nil {
		return fmt.Errorf("clone outcome uncertain for %s; inspect it without deleting/adopting by name: %w", name, err)
	}
	info, err := client.Info(ctx, name)
	if err != nil {
		return fmt.Errorf("successful clone %s needs operator inspection because its immutable ID could not be recorded: %w", name, err)
	}
	id := info.Record.ID
	if id == "" || id == source.ID || info.Record.Name != name {
		return fmt.Errorf("clone %s returned an invalid identity; operator inspection required", name)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cleanupCancel()
		if err := client.Delete(cleanupCtx, id); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("cleanup doctor clone ID %s: %w", id, err))
			return
		}
		machines, err := client.List(cleanupCtx)
		if err == nil {
			for _, machine := range machines {
				if machine.ID == id {
					err = errors.New("doctor clone remains after deletion")
				}
			}
		}
		resultErr = errors.Join(resultErr, err)
	}()
	if info.Record.State != orbstack.StateStopped {
		return errors.New("new doctor clone was not stopped before configuration")
	}
	if err := client.ApplySettings(ctx, name, orbstack.MachineSettings{CPUs: 2, MemoryMiB: 4096, DiskBytes: 68719476736, Isolated: true}); err != nil {
		return err
	}
	if err := client.Start(ctx, id); err != nil {
		return err
	}
	ca, err := os.ReadFile(p.CA)
	if err != nil {
		return err
	}
	if err := client.Run(ctx, orbstack.RunOptions{Machine: id, User: "root", Command: []string{"sh", "-c", "umask 077; cat > /run/garm-orbstack-doctor-ca.pem"}, Stdin: bytes.NewReader(ca), Stdout: io.Discard, Stderr: io.Discard}); err != nil {
		return fmt.Errorf("injecting public doctor CA: %w", err)
	}
	var output bytes.Buffer
	err = client.Run(ctx, orbstack.RunOptions{Machine: id, User: "root", Command: []string{"curl", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "30", "--cacert", "/run/garm-orbstack-doctor-ca.pem", "--output", "/dev/null", "--write-out", "%{http_code}", guestURL + "/api/v1/metadata/runner-metadata"}, Stdout: &output, Stderr: io.Discard})
	if err != nil {
		return fmt.Errorf("isolated clone metadata DNS/TLS/connectivity failed: %w", err)
	}
	if status := strings.TrimSpace(output.String()); status != "401" && status != "403" {
		return fmt.Errorf("metadata endpoint must reject unauthenticated clone with 401/403, got %q", status)
	}
	return nil
}
