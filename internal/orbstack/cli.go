// Package orbstack is the sole orbctl adapter. Every OrbStack interaction
// in this repository goes through this package: absolute orbctl path,
// argument arrays (never host shell strings), context cancellation and JSON
// decoding of machine inventory.
//
// There is no public OrbStack SDK; this adapter wraps the CLI that
// installer preflight resolved to an absolute path.
package orbstack

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"time"
)

// Client wraps one orbctl executable path.
type Client struct {
	OrbctlPath string
}

// New returns a client for the given absolute orbctl path.
func New(orbctlPath string) *Client {
	return &Client{OrbctlPath: orbctlPath}
}

// orbctlEnv builds the environment for every orbctl child. GARM executes
// external providers with a stripped environment (only GARM_* variables
// plus declared provider environment_variables), and orbctl panics
// ("$HOME is not defined") without HOME. The provider block declares
// environment_variables = ["HOME"], and this fallback reconstructs HOME
// from the OS user database when even the provider process lacks it, so
// the adapter works under both the operator shell and GARM's exec.
func orbctlEnv() []string {
	env := os.Environ()
	hasHome := false
	for _, kv := range env {
		if strings.HasPrefix(kv, "HOME=") && len(kv) > len("HOME=") {
			hasHome = true
			break
		}
	}
	if !hasHome {
		if home, err := userHomeDir(); err == nil && home != "" {
			env = append(env, "HOME="+home)
		}
	}
	return env
}

// userHomeDir resolves the current user's home without relying on $HOME.
func userHomeDir() (string, error) {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir, nil
	}
	return "", errors.New("could not resolve home directory")
}

// Image is the distro/arch metadata of a machine.
type Image struct {
	Distro  string `json:"distro"`
	Version string `json:"version"`
	Arch    string `json:"arch"`
	Variant string `json:"variant"`
}

// MachineConfig is the per-machine OrbStack configuration as reported by
// list/info.
type MachineConfig struct {
	Isolated        bool   `json:"isolated"`
	ForwardSSHAgent bool   `json:"forward_ssh_agent"`
	IsolateNetwork  bool   `json:"isolate_network"`
	DefaultUsername string `json:"default_username"`
	HTTPPort        int    `json:"http_port"`
	HTTPSPort       int    `json:"https_port"`
}

// Machine is one machine record from `orbctl list --format json`.
type Machine struct {
	ID      string        `json:"id"`
	Name    string        `json:"name"`
	Image   Image         `json:"image"`
	Config  MachineConfig `json:"config"`
	Builtin bool          `json:"builtin"`
	State   string        `json:"state"`
}

// Info is `orbctl info ID --format json`: the machine record wrapped under
// "record" plus live addressing and disk size.
type Info struct {
	Record   Machine `json:"record"`
	DiskSize int64   `json:"disk_size"`
	IP4      string  `json:"ip4"`
	IP6      string  `json:"ip6"`
}

// Machine states reported by OrbStack.
const (
	StateRunning = "running"
	StateStopped = "stopped"
)

// CreateOptions describes machine creation.
type CreateOptions struct {
	Name           string
	Distro         string // e.g. ubuntu
	Version        string // e.g. 24.04
	Arch           string // arm64 | amd64
	User           string
	Isolated       bool
	IsolateNetwork bool
}

// Create creates a new machine. The machine boots as part of creation.
func (c *Client) Create(ctx context.Context, opts CreateOptions) error {
	args := []string{"create", "--arch", opts.Arch}
	if opts.User != "" {
		args = append(args, "--user", opts.User)
	}
	if opts.Isolated {
		args = append(args, "--isolated")
	}
	if opts.IsolateNetwork {
		args = append(args, "--isolate-network")
	}
	image := opts.Distro
	if opts.Version != "" {
		image += ":" + opts.Version
	}
	args = append(args, image, opts.Name)
	_, err := c.runOutput(ctx, args...)
	return err
}

// CloneCmd returns the prepared `orbctl clone SOURCE DEST` command without
// starting it. Callers may attach ExtraFiles (the inherited instance lock)
// and additional stdio before Run/Wait; this is how a killed helper leaves
// the lock held by the surviving orbctl child.
func (c *Client) CloneCmd(ctx context.Context, source, dest string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.OrbctlPath, "clone", source, dest)
	cmd.Env = orbctlEnv()
	return cmd
}

// Clone clones source to dest and waits for completion. The resulting
// machine is stopped.
func (c *Client) Clone(ctx context.Context, source, dest string) error {
	return c.CloneCmd(ctx, source, dest).Run()
}

// Info returns extended information for a machine by ID or name.
func (c *Client) Info(ctx context.Context, idOrName string) (Info, error) {
	var info Info
	out, err := c.runOutput(ctx, "info", idOrName, "--format", "json")
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return Info{}, fmt.Errorf("decoding orbctl info output: %w", err)
	}
	if info.Record.ID == "" || info.Record.Name == "" {
		return Info{}, fmt.Errorf("orbstack: info for %s returned an empty record", idOrName)
	}
	return info, nil
}

// List returns all machines.
func (c *Client) List(ctx context.Context) ([]Machine, error) {
	out, err := c.runOutput(ctx, "list", "--format", "json")
	if err != nil {
		return nil, err
	}
	var machines []Machine
	if err := json.Unmarshal(out, &machines); err != nil {
		return nil, fmt.Errorf("decoding orbctl list output: %w", err)
	}
	if machines == nil {
		// JSON "null" decodes to a nil slice and would masquerade as a
		// successful empty inventory; release decisions require a
		// complete successful inventory, so reject it.
		return nil, fmt.Errorf("orbstack: list returned null inventory (%q)", firstLine(string(out)))
	}
	seen := make(map[string]struct{}, len(machines))
	for _, m := range machines {
		if m.ID == "" || m.Name == "" {
			return nil, fmt.Errorf("orbstack: list returned a machine with empty ID or name")
		}
		if _, dup := seen[m.ID]; dup {
			return nil, fmt.Errorf("orbstack: list returned duplicate machine ID %s", m.ID)
		}
		seen[m.ID] = struct{}{}
	}
	return machines, nil
}

// Start starts one machine by ID or name.
func (c *Client) Start(ctx context.Context, idOrName string) error {
	_, err := c.runOutput(ctx, "start", idOrName)
	return err
}

// Stop stops one machine by ID or name. Never call Stop with an empty
// target: an argument-free stop would shut down all of OrbStack.
func (c *Client) Stop(ctx context.Context, idOrName string) error {
	if strings.TrimSpace(idOrName) == "" {
		return errors.New("orbstack: refusing stop with empty machine target")
	}
	_, err := c.runOutput(ctx, "stop", idOrName)
	return err
}

// ErrNotFound reports that no machine with the requested ID exists.
var ErrNotFound = errors.New("orbstack: machine not found")

func (c *Client) findByID(ctx context.Context, machineID string) (Machine, error) {
	machines, err := c.List(ctx)
	if err != nil {
		return Machine{}, err
	}
	for _, m := range machines {
		if m.ID == machineID {
			return m, nil
		}
	}
	return Machine{}, ErrNotFound
}

// Delete deletes exactly the machine identified by machineID, and is a no-op
// when that machine no longer exists. It never accepts a caller-supplied
// name for the destructive call.
//
// OrbStack 2.2.3 (the current release and tested baseline) segfaults on
// every `orbctl delete <ID>` form (running/stopped, with and without
// --force) with a nil pointer at scon/cmd/scli/cmd/delete.go:141; only
// name-based deletion works. Instead of weakening ownership checks, the
// delete is composed from ID-addressed operations:
//
//  1. resolve the machine strictly by its recorded ID;
//  2. rename it, addressed by that ID, to a fresh unguessable name — this
//     atomically binds the new name to our ID inside OrbStack, and no other
//     machine can hold that name;
//  3. re-resolve by ID, require the fresh binding to still hold, and
//     delete that exact fresh name.
//
// A foreign machine cannot be hit unless a concurrent local actor renames
// our machine between steps 2 and 3 and recreates the fresh name, which
// requires operator privileges on this Mac. Revisit once OrbStack ships a
// working delete-by-ID.
func (c *Client) Delete(ctx context.Context, machineID string) error {
	if strings.TrimSpace(machineID) == "" {
		return errors.New("orbstack: refusing delete with empty machine ID")
	}
	m, err := c.findByID(ctx, machineID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil // already absent; delete is idempotent
		}
		return err
	}
	// Lift the disk quota on the machine's current name before the rename.
	// A machine at its disk_bytes quota cannot even be renamed (btrfs needs
	// headroom to rewrite metadata), so a lift placed after the rename never
	// runs on exactly the machines that need it. config-set is name-addressed
	// while rename is ID-addressed, so the lift must use the pre-rename name.
	// Setting the limit to 0 (no limit) also gives btrfs the space it needs to
	// destroy subvolumes at delete time. The machine is about to be deleted,
	// so the removed cap has no lasting effect. Best effort: a machine below
	// its quota deletes fine without the lift, and a stuck machine surfaces
	// the underlying ENOSPC from the rename or delete error.
	_, _ = c.runOutput(ctx, "config", "set", "machine."+m.Name+".disk_bytes", "0")
	fresh, err := randomMachineName("garm-del")
	if err != nil {
		return fmt.Errorf("generating deletion name: %w", err)
	}
	if err := c.Rename(ctx, machineID, fresh); err != nil {
		return fmt.Errorf("binding deletion name to machine %s: %w", machineID, err)
	}
	m, err = c.findByID(ctx, machineID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	// The binding we created must still hold. If the name drifted, a
	// concurrent actor is renaming machines; abort rather than delete an
	// unverified name.
	if m.Name != fresh {
		return fmt.Errorf("machine %s renamed concurrently (have %q, want our fresh binding %q); aborting delete", machineID, m.Name, fresh)
	}
	_, err = c.runOutput(ctx, "delete", "--force", fresh)
	return err
}

// Rename renames a machine. The machine ID is stable across rename.
func (c *Client) Rename(ctx context.Context, idOrName, newName string) error {
	_, err := c.runOutput(ctx, "rename", idOrName, newName)
	return err
}

// MachineSettings is the per-machine configuration applied to a stopped
// clone before its first boot. Zero-value/empty fields mean "unset" and are
// written explicitly so clone settings do not silently inherit later host
// defaults.
type MachineSettings struct {
	CPUs            int
	MemoryMiB       int
	DiskBytes       int64
	Isolated        bool
	ForwardSSHAgent bool
	IsolateNetwork  bool
}

// ApplySettings applies per-machine settings via `orbctl config set`.
func (c *Client) ApplySettings(ctx context.Context, machineName string, s MachineSettings) error {
	type setting struct {
		suffix string
		value  string
	}
	settings := []setting{
		{"cpu", strconv.Itoa(s.CPUs)},
		{"memory_mib", strconv.Itoa(s.MemoryMiB)},
		{"disk_bytes", strconv.FormatInt(s.DiskBytes, 10)},
		{"isolated", strconv.FormatBool(s.Isolated)},
		{"forward_ssh_agent", strconv.FormatBool(s.ForwardSSHAgent)},
		{"isolate_network", strconv.FormatBool(s.IsolateNetwork)},
		{"mounts", ""},
	}
	for _, st := range settings {
		if err := c.SetConfig(ctx, "machine."+machineName+"."+st.suffix, st.value); err != nil {
			return fmt.Errorf("setting machine.%s.%s=%s: %w", machineName, st.suffix, st.value, err)
		}
	}
	return nil
}

// SetConfig sets one OrbStack configuration option.
func (c *Client) SetConfig(ctx context.Context, key, value string) error {
	_, err := c.runOutput(ctx, "config", "set", key, value)
	return err
}

// RunOptions describes a command execution inside a machine.
type RunOptions struct {
	Machine string
	User    string
	Workdir string
	Command []string
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
}

// RunCmd returns the prepared `orbctl run` command without starting it.
// Arguments are passed as an argv array; orbctl parses flags
// non-interspersed, so everything after the first guest argument is passed
// through verbatim. Guest commands must therefore start with a program
// name, never a dash; Run enforces this.
func (c *Client) RunCmd(ctx context.Context, opts RunOptions) *exec.Cmd {
	args := []string{"run"}
	if opts.Machine != "" {
		args = append(args, "--machine", opts.Machine)
	}
	if opts.User != "" {
		args = append(args, "--user", opts.User)
	}
	if opts.Workdir != "" {
		args = append(args, "--workdir", opts.Workdir)
	}
	args = append(args, opts.Command...)
	cmd := exec.CommandContext(ctx, c.OrbctlPath, args...)
	cmd.Env = orbctlEnv()
	cmd.Stdin = opts.Stdin
	cmd.Stdout = opts.Stdout
	cmd.Stderr = opts.Stderr
	return cmd
}

// Run executes a command inside a machine, forwarding stdin/stdout/stderr
// and the exit status.
func (c *Client) Run(ctx context.Context, opts RunOptions) error {
	if len(opts.Command) == 0 || strings.HasPrefix(opts.Command[0], "-") {
		return fmt.Errorf("orbstack: guest command must be a non-empty argv starting with a program name, got %q", opts.Command)
	}
	return c.RunCmd(ctx, opts).Run()
}

// ExitError extracts the guest command's exit code from an error returned
// by Run, if available.
func ExitError(err error) (int, bool) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), true
	}
	return 0, false
}

func (c *Client) runOutput(ctx context.Context, args ...string) ([]byte, error) {
	if strings.TrimSpace(c.OrbctlPath) == "" {
		return nil, errors.New("orbstack: orbctl path is empty")
	}
	cmd := exec.CommandContext(ctx, c.OrbctlPath, args...)
	cmd.Env = orbctlEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg != "" {
			return nil, fmt.Errorf("orbctl %s: %s: %w", strings.Join(args, " "), firstLine(msg), err)
		}
		return nil, fmt.Errorf("orbctl %s: %w", strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Version returns the OrbStack version string (for example "2.2.3") via
// the probe-verified `orbctl version` output ("Version: 2.2.3 (2020300)").
func (c *Client) Version(ctx context.Context) (string, error) {
	out, err := c.runOutput(ctx, "version")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "Version:"); ok {
			ver := strings.TrimSpace(rest)
			if i := strings.IndexByte(ver, ' '); i >= 0 {
				ver = ver[:i]
			}
			if ver != "" {
				return ver, nil
			}
		}
	}
	return "", fmt.Errorf("orbstack: could not parse version output %q", strings.TrimSpace(string(out)))
}

// WaitForState polls the machine inventory until the machine reaches the
// wanted state or the context times out.
func (c *Client) WaitForState(ctx context.Context, machineID, want string, interval time.Duration) (Machine, error) {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		machines, err := c.List(ctx)
		if err == nil {
			for _, m := range machines {
				if m.ID == machineID {
					if m.State == want {
						return m, nil
					}
					break
				}
			}
		}
		select {
		case <-ctx.Done():
			return Machine{}, fmt.Errorf("waiting for machine %s to become %s: %w", machineID, want, ctx.Err())
		case <-ticker.C:
		}
	}
}

// randomMachineName returns a fresh unguessable machine name with the
// given prefix, suitable for the rename-composition delete binding.
func randomMachineName(prefix string) (string, error) {
	var b [8]byte
	if _, err := crand.Read(b[:]); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%x", prefix, b), nil
}
