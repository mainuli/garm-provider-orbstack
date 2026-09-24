//go:build integration

// Package integration contains opt-in tests that mutate only disposable
// probe resources. They are excluded from normal builds via the integration
// build tag and additionally gated on GARM_ORBSTACK_INTEGRATION=1.
//
// TestOrbStackCapabilities proves the OrbStack boundary the integration
// depends on before the full provider is trusted:
//
//  1. clone semantics: stopped result, inherited settings, per-clone config
//     before first boot, unique guest identity after machine-id reset,
//     inherited files without shared writes;
//  2. host-to-isolated-guest argv execution with stdin and exit status;
//  3. resource limit enforcement inside an isolated clone;
//  4. guest-local Docker inside an isolated machine;
//  5. isolated-guest-to-Docker HTTPS reachability of a disposable upstream
//     GARM v0.2.1 container via its orb.local domain, with a test CA;
//  6. machine ID stability across stop/start and rename;
//  7. a killed helper's orbctl child retains the inherited instance lock
//     until it exits (fd inheritance survives SIGKILL of the parent).
//
// The test never touches pre-existing machines or containers: every created
// resource carries a unique garm-probe-<random> name and is deleted by ID.
package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"golang.org/x/sys/unix"
)

const (
	garmImage      = "ghcr.io/cloudbase/garm:v0.2.1"
	minOrbstackVer = "2.2.3"
	probeTimeout   = 20 * time.Minute
)

// Child-helper mode: the test binary is re-executed with
// GARM_PROBE_CHILD_LOCK_HELPER=1. It flocks the lock file, passes the fd to
// an orbctl child via ExtraFiles (fd 3), prints HELPER-STARTED and sleeps
// until the parent SIGKILLs it. The orbctl child survives, still holding
// the inherited open file description.
func maybeChildHelper() {
	if os.Getenv("GARM_PROBE_CHILD_LOCK_HELPER") != "1" {
		return
	}
	lockPath := os.Getenv("GARM_PROBE_LOCK_FILE")
	args := strings.Split(os.Getenv("GARM_PROBE_ORBCTL_ARGS"), "\x1f")
	f, err := os.OpenFile(lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintln(os.Stderr, "child: open lock:", err)
		os.Exit(2)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		fmt.Fprintln(os.Stderr, "child: flock:", err)
		os.Exit(2)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.ExtraFiles = []*os.File{f}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "child: start:", err)
		os.Exit(2)
	}
	fmt.Println("HELPER-STARTED")
	for {
		time.Sleep(time.Hour)
	}
}

func TestMain(m *testing.M) {
	maybeChildHelper()
	os.Exit(m.Run())
}

type probeEnv struct {
	t        *testing.T
	client   *orbstack.Client
	suffix   string
	orbctl   string
	created  []string // machine IDs created by this probe
	dockerID string   // probe container name
}

func (p *probeEnv) trackMachine(id string) {
	p.created = append(p.created, id)
}

func (p *probeEnv) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for _, id := range p.created {
		if strings.TrimSpace(id) == "" {
			p.t.Errorf("BUG: cleanup would delete empty machine ID")
			continue
		}
		_ = p.client.Delete(ctx, id)
	}
	if p.dockerID != "" {
		_ = exec.CommandContext(ctx, "docker", "rm", "-f", p.dockerID).Run()
	}
}

func (p *probeEnv) runIn(ctx context.Context, machine, user string, cmd ...string) (string, error) {
	var stdout bytes.Buffer
	err := p.client.Run(ctx, orbstack.RunOptions{
		Machine: machine,
		User:    user,
		Command: cmd,
		Stdout:  &stdout,
		Stderr:  &stdout,
	})
	return stdout.String(), err
}

func (p *probeEnv) runInRoot(ctx context.Context, machine string, cmd ...string) (string, error) {
	return p.runIn(ctx, machine, "root", cmd...)
}

func TestOrbStackCapabilities(t *testing.T) {
	if os.Getenv("GARM_ORBSTACK_INTEGRATION") != "1" {
		t.Skip("GARM_ORBSTACK_INTEGRATION=1 required; this test creates and deletes disposable probe machines")
	}

	// Preflight: OrbStack >= 2.2.3, docker context orbstack.
	out, err := exec.Command("orb", "version").Output()
	if err != nil {
		t.Fatalf("preflight orb version: %v", err)
	}
	ver := strings.TrimSpace(string(out))
	if !strings.Contains(ver, minOrbstackVer) && !newerThan(ver, minOrbstackVer) {
		t.Fatalf("preflight: need OrbStack >= %s, got %q", minOrbstackVer, ver)
	}
	ctxOut, err := exec.Command("docker", "context", "show").Output()
	if err != nil || strings.TrimSpace(string(ctxOut)) != "orbstack" {
		t.Fatalf("preflight: docker context must be orbstack, got %q err=%v", strings.TrimSpace(string(ctxOut)), err)
	}

	orbctlPath, err := exec.LookPath("orbctl")
	if err != nil {
		t.Fatalf("preflight: orbctl not in PATH: %v", err)
	}

	randSuffix, err := randomSuffix()
	if err != nil {
		t.Fatalf("random suffix: %v", err)
	}
	p := &probeEnv{
		t:      t,
		client: orbstack.New(orbctlPath),
		suffix: randSuffix,
		orbctl: orbctlPath,
	}
	t.Cleanup(p.cleanup)

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	source := "garm-probe-" + randSuffix
	clone1 := source + "-c1"
	clone2 := source + "-c2"

	// --- 1. create isolated source machine -------------------------------
	t.Logf("creating isolated source machine %s", source)
	if err := p.client.Create(ctx, orbstack.CreateOptions{
		Name:     source,
		Distro:   "ubuntu",
		Version:  "24.04",
		Arch:     "arm64",
		User:     "runner",
		Isolated: true,
	}); err != nil {
		t.Fatalf("create source: %v", err)
	}
	srcInfo, err := p.client.Info(ctx, source)
	if err != nil {
		t.Fatalf("info source: %v", err)
	}
	p.trackMachine(srcInfo.Record.ID)
	srcMachine, err := p.client.WaitForState(ctx, srcInfo.Record.ID, orbstack.StateRunning, 2*time.Second)
	if err != nil {
		t.Fatalf("source never running: %v", err)
	}
	_ = srcMachine
	if !srcInfo.Record.Config.Isolated {
		t.Fatalf("source machine not isolated: %+v", srcInfo.Record.Config)
	}

	// --- prepare source: packages, marker, docker, identity reset --------
	// Note: OrbStack's ubuntu:24.04 image ships without cloud-init; nothing
	// to wait for. The marker goes to /var/tmp because /tmp does not
	// survive machine stop/clone (tmpfiles cleanup).
	prep := [][]string{
		{"apt-get", "update", "-y"},
		{"apt-get", "install", "-y", "--no-install-recommends", "curl", "ca-certificates", "docker.io"},
		{"systemctl", "enable", "--now", "docker"},
		{"sh", "-c", "echo source-marker-$(hostname) > /var/tmp/garm-probe-marker"},
		// Seal identity: empty /etc/machine-id (systemd first-boot state)
		// and D-Bus linkage reset.
		{"sh", "-c", "rm -f /var/lib/dbus/machine-id && ln -s /etc/machine-id /var/lib/dbus/machine-id && truncate -s 0 /etc/machine-id"},
		// Disable unattended upgrades so package state stays put.
		{"systemctl", "disable", "--now", "apt-daily.timer", "apt-daily-upgrade.timer"},
		{"systemctl", "mask", "unattended-upgrades.service"},
	}
	for _, cmd := range prep {
		if out, err := p.runInRoot(ctx, source, cmd...); err != nil {
			t.Fatalf("source prep %v: %v\n%s", cmd, err, out)
		}
	}

	// Docker must work in the isolated source before cloning.
	if out, err := p.runInRoot(ctx, source, "docker", "run", "--rm", "hello-world"); err != nil {
		t.Fatalf("docker hello-world in isolated source: %v\n%s", err, out)
	}
	t.Log("docker hello-world ok in isolated source")

	// Stop the source: templates must be stopped when cloned.
	if err := p.client.Stop(ctx, srcInfo.Record.ID); err != nil {
		t.Fatalf("stop source: %v", err)
	}
	if _, err := p.client.WaitForState(ctx, srcInfo.Record.ID, orbstack.StateStopped, 2*time.Second); err != nil {
		t.Fatalf("source never stopped: %v", err)
	}

	// --- 2. clone: stopped result, inherited settings ---------------------
	for _, dest := range []string{clone1, clone2} {
		if err := p.client.Clone(ctx, source, dest); err != nil {
			t.Fatalf("clone %s: %v", dest, err)
		}
	}
	for _, dest := range []string{clone1, clone2} {
		info, err := p.client.Info(ctx, dest)
		if err != nil {
			t.Fatalf("info clone %s: %v", dest, err)
		}
		p.trackMachine(info.Record.ID)
		if info.Record.State != orbstack.StateStopped {
			t.Fatalf("clone %s must be stopped, got %s", dest, info.Record.State)
		}
		if !info.Record.Config.Isolated {
			t.Fatalf("clone %s lost isolation: %+v", dest, info.Record.Config)
		}
	}

	// --- 3. per-clone settings before first boot --------------------------
	for _, dest := range []string{clone1, clone2} {
		if err := p.client.ApplySettings(ctx, dest, orbstack.MachineSettings{
			CPUs: 2, MemoryMiB: 2048, DiskBytes: 10 * 1024 * 1024 * 1024,
			Isolated: true, ForwardSSHAgent: false, IsolateNetwork: false,
		}); err != nil {
			t.Fatalf("apply settings %s: %v", dest, err)
		}
	}

	// --- 4. boot clones and verify identity/files/resources ---------------
	var ids [2]string
	for i, dest := range []string{clone1, clone2} {
		info, err := p.client.Info(ctx, dest)
		if err != nil {
			t.Fatalf("info %s: %v", dest, err)
		}
		ids[i] = info.Record.ID
		if err := p.client.Start(ctx, info.Record.ID); err != nil {
			t.Fatalf("start %s: %v", dest, err)
		}
		if _, err := p.client.WaitForState(ctx, info.Record.ID, orbstack.StateRunning, 2*time.Second); err != nil {
			t.Fatalf("%s never running: %v", dest, err)
		}
	}

	var machineIDs [2]string
	for i, dest := range []string{clone1, clone2} {
		out, err := p.runInRoot(ctx, dest, "sh", "-c", "cat /etc/machine-id; hostname")
		if err != nil {
			t.Fatalf("identity in %s: %v\n%s", dest, err, out)
		}
		lines := strings.Fields(out)
		if len(lines) != 2 || lines[0] == "" {
			t.Fatalf("identity in %s: unexpected output %q", dest, out)
		}
		machineIDs[i] = lines[0]
		if lines[1] != dest {
			t.Errorf("hostname in %s: got %q, want %q", dest, lines[1], dest)
		}
	}
	if machineIDs[0] == machineIDs[1] {
		t.Fatalf("clones share /etc/machine-id %q after reset+clone", machineIDs[0])
	}
	t.Logf("distinct machine-id after seal+clone: %s vs %s", machineIDs[0], machineIDs[1])

	// Inherited marker present; writes are not shared between clones.
	for _, dest := range []string{clone1, clone2} {
		out, err := p.runInRoot(ctx, dest, "cat", "/var/tmp/garm-probe-marker")
		if err != nil || !strings.Contains(out, "source-marker") {
			t.Fatalf("inherited marker missing in %s: %v %q", dest, err, out)
		}
	}
	if out, err := p.runInRoot(ctx, clone1, "sh", "-c", "echo c1 > /tmp/garm-probe-c1-only"); err != nil {
		t.Fatalf("write in clone1: %v\n%s", err, out)
	}
	if out, err := p.runInRoot(ctx, clone2, "cat", "/tmp/garm-probe-c1-only"); err == nil {
		t.Fatalf("clone write leaked to sibling: cat succeeded with %q", out)
	}

	// --- 5. stdin + exit status propagation -------------------------------
	var runOut bytes.Buffer
	stdin := "hello-probe-stdin\n"
	runErr := p.client.Run(ctx, orbstack.RunOptions{
		Machine: clone1,
		User:    "runner",
		Command: []string{"sh", "-c", "cat > /tmp/stdin-copy; printf 'bytes='; wc -c < /tmp/stdin-copy; od -An -c /tmp/stdin-copy | head -2; exit 7"},
		Stdin:   strings.NewReader(stdin),
		Stdout:  &runOut,
		Stderr:  &runOut,
	})
	if code, ok := orbstack.ExitError(runErr); !ok || code != 7 {
		t.Fatalf("expected guest exit 7, got err=%v ok=%v code=%d out=%q", runErr, ok, code, runOut.String())
	}
	// "hello-probe-stdin\n" is exactly 18 bytes; od spacing defeats a
	// substring check, so assert the exact byte count.
	if !strings.Contains(runOut.String(), "bytes=18") {
		t.Fatalf("stdin transfer broken: %q", runOut.String())
	}
	t.Logf("stdin transfer exact: %q", strings.TrimSpace(runOut.String()))

	// --- 6. resource enforcement inside the clone -------------------------
	// OrbStack enforces per-machine limits at VM level: vCPU count and VM
	// memory are visible to the guest as nproc and /proc/meminfo.
	resOut, err := p.runInRoot(ctx, clone1, "sh", "-c", "nproc; grep MemTotal /proc/meminfo; cat /sys/fs/cgroup/cpu.max /sys/fs/cgroup/memory.max 2>/dev/null || true")
	if err != nil {
		t.Fatalf("resource check: %v\n%s", err, resOut)
	}
	fields := strings.Fields(resOut)
	if len(fields) < 3 {
		t.Fatalf("resource output: %q", resOut)
	}
	var nproc int
	if _, err := fmt.Sscanf(fields[0], "%d", &nproc); err != nil || nproc > 2 {
		t.Errorf("vCPU limit not enforced: nproc=%s (want <= 2)", fields[0])
	}
	var memTotalKB int
	if _, err := fmt.Sscanf(fields[2], "MemTotal:%d", &memTotalKB); err == nil && memTotalKB > 2048*1024 {
		t.Errorf("memory limit above 2048MiB: MemTotal=%dkB", memTotalKB)
	}
	t.Logf("resource enforcement observed: nproc=%s MemTotal=%skB cgroup=%q", fields[0], fields[2], strings.Join(fields[3:], " "))

	// --- 7. guest-local docker in an isolated clone -----------------------
	dout, err := p.runInRoot(ctx, clone2, "docker", "run", "--rm", "hello-world")
	if err != nil {
		t.Fatalf("docker hello-world in isolated clone: %v\n%s", err, dout)
	}
	t.Log("docker hello-world ok in isolated clone")

	// --- 8. machine ID stability across stop/rename/start ------------------
	if err := p.client.Stop(ctx, ids[0]); err != nil {
		t.Fatalf("stop clone1: %v", err)
	}
	if _, err := p.client.WaitForState(ctx, ids[0], orbstack.StateStopped, 2*time.Second); err != nil {
		t.Fatalf("clone1 never stopped: %v", err)
	}
	renamed := clone1 + "-renamed"
	if err := p.client.Rename(ctx, ids[0], renamed); err != nil {
		t.Fatalf("rename clone1: %v", err)
	}
	if err := p.client.Start(ctx, ids[0]); err != nil {
		t.Fatalf("start renamed clone1: %v", err)
	}
	afterInfo, err := p.client.Info(ctx, ids[0])
	if err != nil {
		t.Fatalf("info renamed: %v", err)
	}
	if afterInfo.Record.ID != ids[0] || afterInfo.Record.Name != renamed {
		t.Fatalf("ID/name stability broken: before id=%s after id=%s name=%s", ids[0], afterInfo.Record.ID, afterInfo.Record.Name)
	}

	// --- 9. GARM HTTPS reachability from an isolated clone -----------------
	testGARMHTTPS(t, ctx, p, renamed)

	// --- 10. killed helper's orbctl child retains the lock -----------------
	testInheritedLockRetention(t, ctx, p, clone2)
}

// testGARMHTTPS starts a disposable upstream GARM v0.2.1 container with a
// test-generated TLS certificate whose SANs include the orb.local container
// domain, then verifies an isolated clone can complete DNS + TCP + TLS and
// receives an HTTP status (401/403 unauthenticated) rather than a network
// error.
func testGARMHTTPS(t *testing.T, ctx context.Context, p *probeEnv, fromMachine string) {
	t.Helper()
	dir := t.TempDir()
	container := "garm.garm-probe-" + p.suffix
	domain := container + ".orb.local"
	caCertPEM, _, serverCertPEM, serverKeyPEM, err := generateTLS(domain)
	if err != nil {
		t.Fatalf("tls generation: %v", err)
	}

	jwtSecret := randomHex(32)
	dbPass := randomHex(16)
	cfg := fmt.Sprintf(`
[default]
enable_webhook_management = false

[logging]
log_format = "text"
log_level = "info"

[jwt_auth]
secret = "%s"

[apiserver]
bind = "0.0.0.0"
port = 9997
use_tls = true
[apiserver.tls]
certificate = "/etc/garm/tls/server-cert.pem"
key = "/etc/garm/tls/server-key.pem"
[apiserver.webui]
enable = false

[database]
backend = "sqlite3"
passphrase = "%s"
[database.sqlite3]
db_file = "/etc/garm/garm.db"
`, jwtSecret, dbPass)

	cfgDir := filepath.Join(dir, "garm")
	if err := os.MkdirAll(filepath.Join(cfgDir, "tls"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "tls", "server-cert.pem"), serverCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "tls", "server-key.pem"), serverKeyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caCertPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	pull := exec.CommandContext(ctx, "docker", "pull", garmImage)
	if out, err := pull.CombinedOutput(); err != nil {
		t.Fatalf("docker pull %s: %v\n%s", garmImage, err, out)
	}
	run := exec.CommandContext(ctx, "docker", "run", "-d", "--name", container,
		"-v", cfgDir+":/etc/garm",
		garmImage)
	var outBuf bytes.Buffer
	run.Stdout = &outBuf
	run.Stderr = &outBuf
	if err := run.Run(); err != nil {
		t.Fatalf("docker run garm probe: %v\n%s", err, outBuf.String())
	}
	p.dockerID = container

	// Wait for GARM to answer TLS from the host via the same orb.local
	// domain, so we know the server is up before testing the guest.
	deadline := time.Now().Add(90 * time.Second)
	up := false
	for time.Now().Before(deadline) {
		cmd := exec.CommandContext(ctx, "curl", "-sS", "--cacert", caPath,
			"-o", "/dev/null", "-w", "%{http_code}",
			"--", "https://"+domain+":9997/api/v1/metadata")
		if out, err := cmd.Output(); err == nil {
			if s := strings.TrimSpace(string(out)); s != "" && s != "000" {
				up = true
				t.Logf("garm probe container up, host-side status %s", s)
				break
			}
		}
		time.Sleep(2 * time.Second)
	}
	if !up {
		logs, _ := exec.CommandContext(ctx, "docker", "logs", container).CombinedOutput()
		t.Fatalf("garm probe never answered; logs:\n%s", logs)
	}

	// Hand the CA to the guest via stdin and probe DNS+TLS+HTTP from there.
	caBytes, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatal(err)
	}
	var guestOut bytes.Buffer
	guestErr := p.client.Run(ctx, orbstack.RunOptions{
		Machine: fromMachine,
		User:    "root",
		Command: []string{"sh", "-c", "cat > /tmp/garm-ca.pem && curl -sS --cacert /tmp/garm-ca.pem -o /dev/null -w '%{http_code}' -- https://" + domain + ":9997/api/v1/metadata"},
		Stdin:   bytes.NewReader(caBytes),
		Stdout:  &guestOut,
		Stderr:  &guestOut,
	})
	status := strings.TrimSpace(guestOut.String())
	if guestErr != nil {
		t.Fatalf("isolated guest cannot reach https://%s:9997 (DNS/TLS/connection failure): %v\n%s", domain, guestErr, guestOut.String())
	}
	switch status {
	case "401", "403", "200":
		t.Logf("guest reached garm over TLS, status %s", status)
	default:
		t.Fatalf("unexpected guest status %q (want 401/403/200), output %q", status, guestOut.String())
	}
}

// testInheritedLockRetention proves fd-based lock inheritance across a
// SIGKILLed helper: the orphaned orbctl child keeps the flock until it
// exits, and an orphaned clone subprocess still completes its machine.
func testInheritedLockRetention(t *testing.T, ctx context.Context, p *probeEnv, spareMachine string) {
	t.Helper()
	lockPath := filepath.Join(t.TempDir(), "instance.lock")

	// (a) deterministic retention: long-running orbctl child holds fd 3.
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(),
		"GARM_PROBE_CHILD_LOCK_HELPER=1",
		"GARM_PROBE_LOCK_FILE="+lockPath,
		"GARM_PROBE_ORBCTL_ARGS="+strings.Join([]string{p.orbctl, "run", "-m", spareMachine, "--", "sleep", "25"}, "\x1f"),
	)
	var childOut bytes.Buffer
	child.Stdout = &childOut
	child.Stderr = &childOut
	if err := child.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	waitStarted := time.Now().Add(30 * time.Second)
	for time.Now().Before(waitStarted) {
		if strings.Contains(childOut.String(), "HELPER-STARTED") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(childOut.String(), "HELPER-STARTED") {
		t.Fatalf("helper never started orbctl: %q", childOut.String())
	}
	if err := child.Process.Kill(); err != nil { // SIGKILL the helper only
		t.Fatalf("kill helper: %v", err)
	}
	_ = child.Wait()

	// Lock must still be held by the surviving orbctl child.
	probe, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock after kill: %v", err)
	}
	defer probe.Close()
	if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("lock was released while the orphaned orbctl child is still alive: inherited-fd design broken")
	} else {
		t.Logf("lock retained by orphaned orbctl child: %v", err)
	}
	// And it must become available once the child exits.
	deadline := time.Now().Add(60 * time.Second)
	for {
		err = unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(int(probe.Fd()), unix.LOCK_UN)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("lock never released after orbctl child exit: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}

	// (b) orphaned clone completes: kill the helper mid-clone, wait for the
	// lock to be released (orbctl exited), then observe the destination.
	orphanDest := "garm-probe-" + p.suffix + "-orphan"
	child2 := exec.Command(os.Args[0])
	child2.Env = append(os.Environ(),
		"GARM_PROBE_CHILD_LOCK_HELPER=1",
		"GARM_PROBE_LOCK_FILE="+lockPath,
		"GARM_PROBE_ORBCTL_ARGS="+strings.Join([]string{p.orbctl, "clone", spareMachine, orphanDest}, "\x1f"),
	)
	var child2Out bytes.Buffer
	child2.Stdout = &child2Out
	child2.Stderr = &child2Out
	if err := child2.Start(); err != nil {
		t.Fatalf("start clone helper: %v", err)
	}
	waitStarted = time.Now().Add(30 * time.Second)
	for time.Now().Before(waitStarted) {
		if strings.Contains(child2Out.String(), "HELPER-STARTED") {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !strings.Contains(child2Out.String(), "HELPER-STARTED") {
		t.Fatalf("clone helper never started: %q", child2Out.String())
	}
	_ = child2.Process.Kill()
	_ = child2.Wait()

	// Wait for the orphaned clone to finish (lock released == orbctl exited).
	probe2, err := os.OpenFile(lockPath, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open lock for clone wait: %v", err)
	}
	defer probe2.Close()
	deadline = time.Now().Add(120 * time.Second)
	for {
		err = unix.Flock(int(probe2.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			_ = unix.Flock(int(probe2.Fd()), unix.LOCK_UN)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("orphaned clone never released the lock: %v", err)
		}
		time.Sleep(500 * time.Millisecond)
	}
	// The clone must eventually exist as a stopped machine (it completed
	// despite the helper being killed).
	exists := false
	deadline = time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		machines, lerr := p.client.List(ctx)
		if lerr == nil {
			for _, m := range machines {
				if m.Name == orphanDest {
					if m.ID != "" {
						p.trackMachine(m.ID)
					}
					if m.State == orbstack.StateStopped {
						exists = true
					}
				}
			}
		}
		if exists {
			break
		}
		time.Sleep(time.Second)
	}
	if !exists {
		t.Fatal("orphaned clone did not complete: machine missing or not stopped after lock release")
	}
	t.Log("orphaned clone completed after helper kill; lock held for its full lifetime")
}

// generateTLS creates a test CA and a server certificate valid for the
// probe's orb.local container domain and localhost.
func generateTLS(domain string) (caCert, caKey, serverCert, serverKey []byte, err error) {
	caRSA, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "garm-probe-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caRSA.PublicKey, caRSA)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	caCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(caRSA)})
	caCertParsed, _ := x509.ParseCertificate(caDER)

	serverRSA, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "garm-probe"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", "garm.garm-orbstack.orb.local", domain},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCertParsed, &serverRSA.PublicKey, caRSA)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	serverCertPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverRSA)})
	return caCertPEM, caKeyPEM, serverCertPEM, serverKeyPEM, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%x", b)
}

func randomSuffix() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", b), nil
}

func newerThan(got, min string) bool {
	var gotMajor, gotMinor, gotPatch, minMajor, minMinor, minPatch int
	if _, err := fmt.Sscanf(got, "Version: %d.%d.%d", &gotMajor, &gotMinor, &gotPatch); err != nil {
		return false
	}
	if _, err := fmt.Sscanf(min, "%d.%d.%d", &minMajor, &minMinor, &minPatch); err != nil {
		return false
	}
	switch {
	case gotMajor != minMajor:
		return gotMajor > minMajor
	case gotMinor != minMinor:
		return gotMinor > minMinor
	default:
		return gotPatch >= minPatch
	}
}
