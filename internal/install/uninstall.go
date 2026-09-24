package install

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"

	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
)

func Uninstall(ctx context.Context, configPath string) error {
	if err := ValidateReleaseMetadata(); err != nil {
		return err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	p, err := pathsFor(home, configPath, releaseinfo.Version)
	if err != nil {
		return err
	}
	if err := p.check(); err != nil {
		return err
	}
	lock, err := lockInstallation(p)
	if err != nil {
		return err
	}
	defer lock.Close()
	record, err := loadInstallation(p)
	if err != nil {
		return err
	}
	if record.Version != releaseinfo.Version || record.PreviousVersion != "" {
		return errors.New("finish or inspect the pending version change before uninstalling")
	}
	host, err := config.LoadHost(p.HostConfig)
	if err != nil {
		return err
	}
	if record.ControllerID != host.ControllerID || host.StateDir != p.State {
		return errors.New("installation/controller ownership mismatch")
	}
	records, err := state.Snapshot(ctx, host.StateDir, host.ControllerID)
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return errors.New("refusing uninstall while managed runner records remain; disable/drain scale sets and delete runners through GARM (inspect pending reservations with doctor/recover)")
	}
	if err := validateManagedLink(p, record.Version); err != nil {
		return err
	}
	if data, err := os.ReadFile(p.Plist); err == nil {
		if !bytes.Equal(data, renderPlist(p)) {
			return errors.New("refusing removal of an altered/unmanaged LaunchAgent")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := os.Lstat(p.Release); err == nil {
		if _, err := verifyRelease(p.Release, releaseinfo.Version, runtime.GOARCH, true); err != nil {
			return fmt.Errorf("refusing removal of an altered/unmanaged release: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	launchctl, err := absoluteExecutable("launchctl")
	if err != nil {
		return err
	}
	if err := stopService(ctx, launchctl); err != nil {
		return err
	}
	// Check once more after the controller is stopped, before deleting its tools.
	records, err = state.Snapshot(ctx, host.StateDir, host.ControllerID)
	if err != nil {
		return err
	}
	if len(records) != 0 {
		return errors.New("runner records appeared while stopping; controller is stopped, managed data preserved")
	}
	for _, path := range []string{p.Plist, p.Link} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.RemoveAll(p.Release); err != nil {
		return err
	}
	if err := os.RemoveAll(p.CLIHome); err != nil {
		return err
	}
	fmt.Fprintln(os.Stdout, "Uninstalled the LaunchAgent, this release, managed symlink, and isolated CLI profile. Preserved controller DB, registry, templates, host configuration, CA and encryption secrets. Reinstall uses recovery login, never controller init.")
	return nil
}
