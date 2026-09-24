package install

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"golang.org/x/sys/unix"
)

func renderGARMConfig(p Paths, secrets credentials, provider bool) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, `[apiserver]
bind = "127.0.0.1"
port = 9997
use_tls = true
[apiserver.tls]
certificate = %q
key = %q
[jwt_auth]
secret = %q
time_to_live = "8760h"
[database]
backend = "sqlite3"
passphrase = %q
[database.sqlite3]
db_file = %q
`, p.Certificate, p.Key, secrets.JWTSecret, secrets.DatabasePassphrase, filepath.Join(p.State, "garm.db"))
	if provider {
		fmt.Fprintf(&b, `[[provider]]
name = "orbstack"
provider_type = "external"
description = "OrbStack runner provider"
[provider.external]
provider_executable = %q
config_file = %q
interface_version = "v0.1.0"
environment_variables = ["HOME"]
`, filepath.Join(p.Release, "providers.d/garm-provider-orbstack"), p.HostConfig)
	}
	return []byte(b.String())
}

func renderPlist(p Paths) []byte {
	escape := func(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
	return []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>--config</string><string>%s</string></array>
<key>WorkingDirectory</key><string>%s</string>
<key>EnvironmentVariables</key><dict><key>HOME</key><string>%s</string><key>PATH</key><string>/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
<key>RunAtLoad</key><true/>
<key>KeepAlive</key><true/>
<key>Umask</key><integer>63</integer>
<key>StandardOutPath</key><string>%s</string>
<key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, serviceLabel, escape(filepath.Join(p.Release, "garm")), escape(p.GARMConfig), escape(p.State), escape(p.Home), escape(filepath.Join(p.State, "controller.stdout.log")), escape(filepath.Join(p.State, "controller.stderr.log"))))
}

func outputCommand(ctx context.Context, executable string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, args...)
	return cmd.Output()
}

func absoluteExecutable(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return path, checkExecutable(path)
}

func checkExecutable(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("executable must be absolute: %s", path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable: %s", path)
	}
	return nil
}

var orbVersionPattern = regexp.MustCompile(`\b([0-9]+)\.([0-9]+)\.([0-9]+)\b`)

func compatibleOrbVersion(output string) bool {
	m := orbVersionPattern.FindStringSubmatch(output)
	if len(m) != 4 {
		return false
	}
	want := []int{2, 2, 3}
	for i := range want {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return false
		}
		if n != want[i] {
			return n > want[i]
		}
	}
	return true
}

func preflight(ctx context.Context) (string, string, error) {
	if runtime.GOOS != "darwin" || (runtime.GOARCH != "arm64" && runtime.GOARCH != "amd64") {
		return "", "", errors.New("installation requires macOS arm64 or amd64")
	}
	if os.Geteuid() == 0 {
		return "", "", errors.New("run as the logged-in OrbStack owner, not root or sudo")
	}
	if _, err := absoluteExecutable("orb"); err != nil {
		return "", "", fmt.Errorf("OrbStack prerequisite: %w", err)
	}
	orbctl, err := absoluteExecutable("orbctl")
	if err != nil {
		return "", "", err
	}
	versionCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	version, err := orbstack.New(orbctl).Version(versionCtx)
	cancel()
	if err != nil || !compatibleOrbVersion(version) {
		return "", "", fmt.Errorf("OrbStack 2.2.3 or newer must be installed and running (version check: %v)", err)
	}
	launchctl, err := absoluteExecutable("launchctl")
	if err != nil {
		return "", "", err
	}
	if _, err := outputCommand(ctx, launchctl, "print", launchDomain()); err != nil {
		return "", "", errors.New("a logged-in macOS GUI user session is required for the LaunchAgent")
	}
	return orbctl, launchctl, nil
}

func launchDomain() string { return "gui/" + strconv.Itoa(os.Getuid()) }
func launchTarget() string { return launchDomain() + "/" + serviceLabel }

func launchStatus(ctx context.Context, launchctl string) ([]byte, bool, error) {
	out, err := outputCommand(ctx, launchctl, "print", launchTarget())
	if err == nil {
		return out, true, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), "Could not find service") {
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("reading LaunchAgent status: %w", err)
}

// startService ensures the controller is running and returns whether it
// BOOTSTRAPPED the job (it was not loaded before). A freshly bootstrapped
// process has already loaded the current on-disk configuration, so callers
// must not treat pre-existing config changes as needing another restart.
func startService(ctx context.Context, launchctl string, p Paths, restart bool) (bool, error) {
	status, loaded, err := launchStatus(ctx, launchctl)
	if err != nil {
		return false, err
	}
	pid, err := launchPID(status)
	if err != nil {
		return false, err
	}
	if !loaded {
		if _, err := outputCommand(ctx, launchctl, "bootstrap", launchDomain(), p.Plist); err != nil {
			return false, fmt.Errorf("bootstrap LaunchAgent: %w", err)
		}
	}
	args := []string{"kickstart"}
	if restart {
		args = append(args, "-k")
	}
	args = append(args, launchTarget())
	if _, err := outputCommand(ctx, launchctl, args...); err != nil {
		return false, fmt.Errorf("kickstart LaunchAgent: %w", err)
	}
	if restart {
		return false, waitProcessExit(ctx, pid)
	}
	return !loaded, nil
}

func stopService(ctx context.Context, launchctl string) error {
	status, loaded, err := launchStatus(ctx, launchctl)
	if err != nil || !loaded {
		return err
	}
	pid, err := launchPID(status)
	if err != nil {
		return err
	}
	if _, err := outputCommand(ctx, launchctl, "bootout", launchTarget()); err != nil {
		return fmt.Errorf("bootout LaunchAgent: %w", err)
	}
	_, loaded, err = launchStatus(ctx, launchctl)
	if err != nil {
		return err
	}
	if loaded {
		return errors.New("LaunchAgent remains loaded; refusing to mutate or back up live controller data")
	}
	return waitProcessExit(ctx, pid)
}

func launchPID(status []byte) (int, error) {
	pid := 0
	for _, line := range strings.Split(string(status), "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "pid = "); ok {
			var err error
			pid, err = strconv.Atoi(value)
			if err != nil || pid < 1 {
				return 0, errors.New("cannot establish controller PID")
			}
		}
	}
	if strings.Contains(string(status), "state = running") && pid == 0 {
		return 0, errors.New("running LaunchAgent did not report a controller PID")
	}
	return pid, nil
}

func waitProcessExit(ctx context.Context, pid int) error {
	if pid == 0 {
		return nil
	}
	deadline, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		err := unix.Kill(pid, 0)
		if errors.Is(err, unix.ESRCH) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("checking stopped controller PID: %w", err)
		}
		select {
		case <-deadline.Done():
			return errors.New("previous controller PID remains alive; refusing live-data backup/removal or stale readiness")
		case <-tick.C:
		}
	}
}

func httpClient(p Paths) (*http.Client, error) {
	roots, err := installationRoots(p.CA)
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return errors.New("unexpected controller redirect") },
	}, nil
}

func probeHTTP(ctx context.Context, p Paths, path string) (int, error) {
	client, err := httpClient(p)
	if err != nil {
		return 0, err
	}
	defer client.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://127.0.0.1:9997"+path, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 65536))
	return resp.StatusCode, nil
}

func waitController(ctx context.Context, p Paths) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var last error
	for {
		status, err := probeHTTP(ctx, p, "/api/v1/controller")
		if err == nil {
			if status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusConflict {
				return status, nil
			}
			err = fmt.Errorf("unexpected controller status %d", status)
		}
		last = err
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("waiting for CA-validated controller HTTPS: %w", errors.Join(ctx.Err(), last))
		case <-ticker.C:
		}
	}
}
