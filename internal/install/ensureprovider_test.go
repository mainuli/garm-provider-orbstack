package install

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stageProviderFixture builds a Paths whose release dir contains a fake
// garm-cli (scriptBody) plus a valid authenticated managed profile, and a
// fake launchctl that logs its invocations.
func stageProviderFixture(t *testing.T, cliScript string) (Paths, string, error) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := pathsFor(home, "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{p.Release, filepath.Dir(profilePath(p))} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	cli := filepath.Join(p.Release, "garm-cli")
	if err := os.WriteFile(cli, []byte("#!/bin/sh\n"+cliScript), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := "active_manager = \"garm-orbstack\"\n[[manager]]\nname = \"garm-orbstack\"\nbase_url = \"https://localhost:9997\"\nbearer_token = \"tok\"\n"
	if err := os.WriteFile(profilePath(p), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(t.TempDir(), "launchctl.log")
	return p, log, nil
}

func kickstartCount(t *testing.T, log string) int {
	t.Helper()
	raw, err := os.ReadFile(log)
	if errors.Is(err, os.ErrNotExist) {
		return 0 // launchctl never invoked: the strongest no-restart proof
	}
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "kickstart") {
			n++
		}
	}
	return n
}

// TestEnsureProviderQueryErrorNeverRestarts: a CLI query failure (timeout,
// auth, malformed output) must NOT restart the controller — kickstart -k
// would kill in-flight provider clones.
func TestEnsureProviderQueryErrorNeverRestarts(t *testing.T) {
	p, log, _ := stageProviderFixture(t, "exit 1\n")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err := ensureProvider(ctx, fakeLaunchctlPath(t, log), p)
	if err == nil || !strings.Contains(err.Error(), "no restart") {
		t.Fatalf("expected query error, got %v", err)
	}
	if n := kickstartCount(t, log); n != 0 {
		t.Fatalf("query error must not restart the controller; kickstart called %d times", n)
	}
}

// TestEnsureProviderAbsentRegistrationRestartsOnce: only a successful
// provider-list response lacking the orbstack entry triggers exactly one
// restart.
func TestEnsureProviderAbsentRegistrationRestartsOnce(t *testing.T) {
	p, log, _ := stageProviderFixture(t, "echo '[]'\n")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	launchctl := fakeLaunchctlPath(t, log)
	err := ensureProvider(ctx, launchctl, p)
	if err == nil || !errors.Is(err, errProviderNotRegistered) {
		t.Fatalf("expected not-registered error, got %v", err)
	}
	if n := kickstartCount(t, log); n != 1 {
		t.Fatalf("expected exactly one kickstart, got %d", n)
	}
}

// TestEnsureProviderRegisteredNoRestart: healthy registration touches
// nothing.
func TestEnsureProviderRegisteredNoRestart(t *testing.T) {
	p, log, _ := stageProviderFixture(t, "echo '[{\"name\":\"orbstack\"}]'\n")
	if err := ensureProvider(t.Context(), fakeLaunchctlPath(t, log), p); err != nil {
		t.Fatalf("registered provider must pass: %v", err)
	}
	if n := kickstartCount(t, log); n != 0 {
		t.Fatalf("healthy state must not restart; kickstart called %d times", n)
	}
}

func fakeLaunchctlPath(t *testing.T, log string) string {
	t.Helper()
	launchctl := filepath.Join(t.TempDir(), "launchctl")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$1\" in\n  print) echo 'pid = 999999' ;;\nesac\nexit 0\n"
	if err := os.WriteFile(launchctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return launchctl
}

// fakeSlowLaunchctl creates a launchctl stub that simulates launchd's
// asynchronous KeepAlive removal: after a bootout, `print` still reports
// the job loaded (pid = N) for delayMore calls, then switches to
// "Could not find service" (exit non-zero), which launchStatus treats as
// unloaded. This drives stopService's bounded poll.
func fakeSlowLaunchctl(t *testing.T, log string, delayMore int) string {
	t.Helper()
	launchctl := filepath.Join(t.TempDir(), "launchctl-slow")
	script := "#!/bin/sh\necho \"$@\" >> " + log + "\ncase \"$1\" in\n" +
		"  bootout)\n    echo " + strconv.Itoa(delayMore) + " > \"$LAUNCHCTL_DELAY_FILE\"\n    ;;\n" +
		"  print)\n    if [ -f \"$LAUNCHCTL_DELAY_FILE\" ]; then\n      n=$(cat \"$LAUNCHCTL_DELAY_FILE\")\n      if [ \"$n\" -gt 0 ]; then\n        echo $((n-1)) > \"$LAUNCHCTL_DELAY_FILE\"\n        echo 'pid = 999999'\n        exit 0\n      fi\n      echo 'Could not find service' >&2\n      exit 1\n    fi\n    echo 'pid = 999999'\n    exit 0\n    ;;\n" +
		"esac\nexit 0\n"
	if err := os.WriteFile(launchctl, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return launchctl
}

// TestStopServicePollsThroughSlowBootout proves the bounded poll: the fake
// launchctl reports the job loaded for 4 more calls after bootout, then
// reports it gone. stopService must succeed rather than refusing at the
// first still-loaded check (the pre-fix behaviour that raced the real
// v0.1.0→v0.2.0 upgrade).
func TestStopServicePollsThroughSlowBootout(t *testing.T) {
	log := filepath.Join(t.TempDir(), "launchctl-slow.log")
	delayFile := filepath.Join(t.TempDir(), "bootout-delay")
	t.Setenv("LAUNCHCTL_DELAY_FILE", delayFile)
	home := t.TempDir()
	t.Setenv("HOME", home)
	p, err := pathsFor(home, "", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	_ = p
	launchctl := fakeSlowLaunchctl(t, log, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := stopService(ctx, launchctl); err != nil {
		t.Fatalf("stopService must poll through the async bootout, got: %v", err)
	}
	// The poll must have actually cycled: at least bootout + 4 prints + a
	// final print that errors.
	raw, _ := os.ReadFile(log)
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) < 6 {
		t.Fatalf("expected at least 6 launchctl invocations (bootout + 5 prints), got %d:\n%s", len(lines), string(raw))
	}
}
