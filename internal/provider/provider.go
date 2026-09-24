// Package provider implements GARM's v0.1.0 lifecycle using only operator-owned
// registry records. Machine names are never authority to adopt or delete.
package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	gerrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/execution/common"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/google/uuid"
	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/releaseinfo"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

type machineClient interface {
	CloneCmd(context.Context, string, string) *exec.Cmd
	Info(context.Context, string) (orbstack.Info, error)
	List(context.Context) ([]orbstack.Machine, error)
	ApplySettings(context.Context, string, orbstack.MachineSettings) error
	Start(context.Context, string) error
	Stop(context.Context, string) error
	Delete(context.Context, string) error
	Run(context.Context, orbstack.RunOptions) error
}

type Provider struct {
	host     config.Host
	poolID   string
	registry *state.Registry
	orb      machineClient
}

var _ common.ExternalProvider = (*Provider)(nil)

// New authorizes the controller before opening the registry or invoking OrbStack.
// poolID is opaque and may be empty for the SDK's ID-addressed operations.
func New(ctx context.Context, host config.Host, controllerID, poolID string) (*Provider, error) {
	requested, err := uuid.Parse(controllerID)
	if err != nil {
		return nil, errors.New("invalid controller identity")
	}
	configured, err := uuid.Parse(host.ControllerID)
	if err != nil || requested != configured {
		return nil, errors.New("controller identity does not match host configuration")
	}
	registry, err := state.Open(ctx, host.StateDir, host.ControllerID)
	if err != nil {
		return nil, err
	}
	return &Provider{host: host, poolID: poolID, registry: registry, orb: orbstack.New(host.OrbctlPath)}, nil
}

func (p *Provider) GetVersion(context.Context) string { return releaseinfo.Version }

func generatedName(prefix string) (string, error) {
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]), nil
}

func (p *Provider) CreateInstance(ctx context.Context, bootstrap params.BootstrapInstance) (params.ProviderInstance, error) {
	if strings.TrimSpace(bootstrap.Name) == "" || strings.ContainsRune(bootstrap.Name, 0) {
		return params.ProviderInstance{}, errors.New("runner name is required")
	}
	if p.poolID == "" || bootstrap.PoolID != p.poolID {
		return params.ProviderInstance{}, errors.New("bootstrap pool does not match invocation pool")
	}
	if bootstrap.OSType != params.Linux {
		return params.ProviderInstance{}, errors.New("only Linux runners are supported")
	}
	if bootstrap.OSArch != params.Arm64 && bootstrap.OSArch != params.Amd64 {
		return params.ProviderInstance{}, errors.New("only arm64 and amd64 runners are supported")
	}
	image, ok := p.host.Images[bootstrap.Image]
	if !ok {
		return params.ProviderInstance{}, errors.New("unknown registered image")
	}
	if image.Arch != string(bootstrap.OSArch) {
		return params.ProviderInstance{}, errors.New("runner architecture does not match registered image")
	}
	flavor, ok := p.host.Flavors[bootstrap.Flavor]
	if !ok {
		return params.ProviderInstance{}, errors.New("unknown runner flavor")
	}
	manifest, err := templates.LoadManifest(image.ManifestPath)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	if manifest.ImageID != bootstrap.Image || manifest.MachineID != image.MachineID || manifest.Arch != image.Arch {
		return params.ProviderInstance{}, errors.New("registered image and template manifest disagree")
	}
	plan, err := prepareBootstrap(bootstrap, manifest)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	lock, err := p.registry.LockInstance(ctx, bootstrap.Name)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	defer lock.Close()
	name, err := generatedName("garm-runner-")
	if err != nil {
		return params.ProviderInstance{}, err
	}
	record, fresh, err := p.registry.Reserve(ctx, state.Record{
		SchemaVersion: state.SchemaVersion, ControllerID: p.host.ControllerID,
		PoolID: bootstrap.PoolID, RunnerName: bootstrap.Name, MachineName: name,
		ImageID: bootstrap.Image, Flavor: bootstrap.Flavor, Phase: state.PhaseReserved,
	}, p.host.MaxInstances)
	if err != nil {
		if errors.Is(err, state.ErrConflict) {
			return params.ProviderInstance{}, fmt.Errorf("runner name belongs to a different request: %w", gerrors.ErrDuplicateEntity)
		}
		return params.ProviderInstance{}, err
	}
	if !fresh {
		if record.Phase != state.PhaseActive {
			return params.ProviderInstance{}, fmt.Errorf("runner creation is incomplete; delete or recover its reservation: %w", gerrors.ErrDuplicateEntity)
		}
		return p.observe(ctx, record)
	}
	created, err := p.clone(ctx, record, image, manifest, lock)
	if created.MachineID != "" {
		record = created
	}
	fail := func(cause error) (params.ProviderInstance, error) {
		// Cleanup is bounded independently when the original operation expired.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.host.OperationTimeout)
		defer cancel()
		if cleanupErr := p.deleteRecord(cleanupCtx, record, false); cleanupErr != nil {
			return params.ProviderInstance{}, fmt.Errorf("%w; owned machine retained for deletion: %v", cause, cleanupErr)
		}
		return params.ProviderInstance{}, cause
	}
	if err != nil {
		if record.MachineID != "" {
			return fail(err)
		}
		return params.ProviderInstance{}, err
	}
	info, err := p.ownedInfo(ctx, record)
	if err != nil {
		return fail(err)
	}
	if info.Record.State != orbstack.StateStopped {
		return fail(errors.New("new clone is not stopped before settings"))
	}
	if err := p.orb.ApplySettings(ctx, info.Record.Name, orbstack.MachineSettings{
		CPUs: flavor.CPUs, MemoryMiB: flavor.MemoryMiB, DiskBytes: flavor.DiskBytes,
		Isolated: true, ForwardSSHAgent: false, IsolateNetwork: false,
	}); err != nil {
		return fail(err)
	}
	if err := p.orb.Start(ctx, record.MachineID); err != nil {
		return fail(err)
	}
	record.Phase = state.PhaseBootstrapping
	if err := p.registry.Save(ctx, record); err != nil {
		return fail(err)
	}
	if err := p.injectBootstrap(ctx, record.MachineID, plan); err != nil {
		return fail(err)
	}
	result, err := p.observe(ctx, record)
	if err != nil {
		return fail(err)
	}
	record.Phase = state.PhaseActive
	if err := p.registry.Save(ctx, record); err != nil {
		// Enqueue succeeded. Never enqueue twice, even if this commit's outcome
		// is uncertain; retain ownership for reconciliation/deletion.
		return params.ProviderInstance{}, fmt.Errorf("bootstrap queued but active commit failed; inspect owned runner: %w", err)
	}
	return result, nil
}

func (p *Provider) clone(ctx context.Context, record state.Record, image config.Image, manifest templates.Manifest, instanceLock *state.Lock) (state.Record, error) {
	templateLock, err := p.registry.LockTemplate(ctx, image.MachineID)
	if err != nil {
		return state.Record{}, err
	}
	defer templateLock.Close()
	beforeCloneFailure := func(cause error) (state.Record, error) {
		if err := p.registry.Remove(ctx, record.RunnerName); err != nil {
			return state.Record{}, errors.Join(cause, err)
		}
		return state.Record{}, cause
	}
	template, err := p.orb.Info(ctx, image.MachineID)
	if err != nil {
		return beforeCloneFailure(err)
	}
	if err := templates.ValidateMachine(manifest, template.Record); err != nil {
		return beforeCloneFailure(err)
	}
	inventory, err := p.inventory(ctx)
	if err != nil {
		return beforeCloneFailure(err)
	}
	for _, machine := range inventory {
		if machine.Name == record.MachineName {
			return beforeCloneFailure(errors.New("generated clone name already exists"))
		}
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return beforeCloneFailure(err)
	}
	defer devnull.Close()
	cmd := p.orb.CloneCmd(ctx, image.MachineID, record.MachineName)
	cmd.ExtraFiles = []*os.File{instanceLock.File}
	// File-backed stdio is required for the probe-verified inherited-lock
	// behavior; no pipe can strand Wait after a spawning helper is killed.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	if err := p.registry.MarkCloneIntent(record.RunnerName); err != nil {
		return state.Record{}, err
	}
	if err := cmd.Start(); err != nil {
		if clearErr := p.registry.ClearCloneIntent(record.RunnerName); clearErr != nil {
			return state.Record{}, errors.Join(err, clearErr)
		}
		return beforeCloneFailure(fmt.Errorf("starting template clone: %w", err))
	}
	if err := cmd.Wait(); err != nil {
		return state.Record{}, fmt.Errorf("clone outcome uncertain; inspect reservation %q and use recover: %w", record.RunnerName, err)
	}
	info, err := p.orb.Info(ctx, record.MachineName)
	if err != nil {
		return state.Record{}, fmt.Errorf("clone completed but ID is unrecorded; explicit recovery required: %w", err)
	}
	if info.Record.Name != record.MachineName || info.Record.ID == "" || p.isTemplate(info.Record.ID) {
		return state.Record{}, errors.New("clone identity is uncertain; explicit recovery required")
	}
	record.MachineID, record.Phase = info.Record.ID, state.PhaseCreated
	if err := p.registry.Save(ctx, record); err != nil {
		return state.Record{}, fmt.Errorf("clone ID commit uncertain; retain machine %s for recovery: %w", record.MachineID, err)
	}
	if err := p.registry.ClearCloneIntent(record.RunnerName); err != nil {
		return record, fmt.Errorf("clone recorded but intent cleanup failed: %w", err)
	}
	return record, nil
}

func (p *Provider) isTemplate(id string) bool {
	for _, image := range p.host.Images {
		if image.MachineID == id {
			return true
		}
	}
	return false
}

func (p *Provider) inventory(ctx context.Context) ([]orbstack.Machine, error) {
	machines, err := p.orb.List(ctx)
	if err != nil {
		return nil, err
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, machine := range machines {
		if machine.ID == "" || machine.Name == "" || ids[machine.ID] || names[machine.Name] {
			return nil, errors.New("OrbStack returned an incomplete or ambiguous inventory")
		}
		ids[machine.ID], names[machine.Name] = true, true
	}
	return machines, nil
}

func (p *Provider) ownedInfo(ctx context.Context, record state.Record) (orbstack.Info, error) {
	if record.ControllerID != p.host.ControllerID || record.MachineID == "" || p.isTemplate(record.MachineID) {
		return orbstack.Info{}, errors.New("refusing unowned or template target")
	}
	machines, err := p.inventory(ctx)
	if err != nil {
		return orbstack.Info{}, err
	}
	present := false
	for _, machine := range machines {
		if machine.ID == record.MachineID {
			present = true
			break
		}
	}
	if !present {
		return orbstack.Info{}, gerrors.ErrNotFound
	}
	info, err := p.orb.Info(ctx, record.MachineID)
	if err != nil {
		return orbstack.Info{}, err
	}
	if info.Record.ID != record.MachineID {
		return orbstack.Info{}, errors.New("OrbStack machine identity changed")
	}
	return info, nil
}

func instance(record state.Record, info orbstack.Info) params.ProviderInstance {
	result := params.ProviderInstance{ProviderID: record.MachineID, Name: record.RunnerName,
		OSType: params.Linux, OSName: info.Record.Image.Distro, OSVersion: info.Record.Image.Version,
		OSArch: params.OSArch(info.Record.Image.Arch), Status: params.InstanceStatusUnknown}
	switch info.Record.State {
	case orbstack.StateRunning:
		result.Status = params.InstanceRunning
	case orbstack.StateStopped:
		result.Status = params.InstanceStopped
	case "error", "fault", "failed":
		result.Status = params.InstanceError
	}
	for _, ip := range []string{info.IP4, info.IP6} {
		if ip != "" {
			result.Addresses = append(result.Addresses, params.Address{Address: ip, Type: params.PrivateAddress})
		}
	}
	return result
}

func (p *Provider) observe(ctx context.Context, record state.Record) (params.ProviderInstance, error) {
	info, err := p.ownedInfo(ctx, record)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	return instance(record, info), nil
}

func (p *Provider) resolve(ctx context.Context, target string) (state.Record, error) {
	if strings.TrimSpace(target) == "" {
		return state.Record{}, errors.New("empty instance target")
	}
	record, err := p.registry.Find(ctx, target)
	if errors.Is(err, state.ErrNotFound) {
		return state.Record{}, gerrors.ErrNotFound
	}
	if err != nil {
		return state.Record{}, err
	}
	if p.poolID != "" && record.PoolID != p.poolID {
		return state.Record{}, errors.New("instance belongs to another pool")
	}
	return record, nil
}

// locked resolves a target twice to prevent acting on stale registry ownership
// while another lifecycle operation is removing or reusing a runner name.
func (p *Provider) locked(ctx context.Context, target string, wait bool) (state.Record, *state.Lock, error) {
	record, err := p.resolve(ctx, target)
	if err != nil {
		return state.Record{}, nil, err
	}
	var lock *state.Lock
	if wait {
		lock, err = p.registry.LockInstance(ctx, record.RunnerName)
	} else {
		lock, err = p.registry.TryLockInstance(ctx, record.RunnerName)
	}
	if err != nil {
		return state.Record{}, nil, err
	}
	current, err := p.resolve(ctx, target)
	if err != nil {
		lock.Close()
		return state.Record{}, nil, err
	}
	if current.RunnerName != record.RunnerName || current.MachineName != record.MachineName {
		lock.Close()
		return state.Record{}, nil, errors.New("runner ownership changed during operation")
	}
	return current, lock, nil
}

func (p *Provider) GetInstance(ctx context.Context, target string) (params.ProviderInstance, error) {
	record, lock, err := p.locked(ctx, target, true)
	if err != nil {
		return params.ProviderInstance{}, err
	}
	defer lock.Close()
	if record.MachineID == "" {
		return params.ProviderInstance{}, gerrors.ErrNotFound
	}
	return p.observe(ctx, record)
}

func (p *Provider) ListInstances(ctx context.Context, poolID string) ([]params.ProviderInstance, error) {
	if poolID == "" || (p.poolID != "" && p.poolID != poolID) {
		return nil, errors.New("list requires the invocation pool identity")
	}
	records, err := p.registry.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	machines, err := p.inventory(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]orbstack.Machine, len(machines))
	for _, machine := range machines {
		byID[machine.ID] = machine
	}
	result := make([]params.ProviderInstance, 0)
	for _, record := range records {
		if record.PoolID != poolID || record.MachineID == "" {
			continue
		}
		machine, ok := byID[record.MachineID]
		if !ok {
			continue
		}
		if p.isTemplate(record.MachineID) {
			return nil, errors.New("registry illegally assigns a template as a runner")
		}
		// List's successful inventory is the authoritative observed snapshot.
		// Extended IPs are supplied by Create/Get without an N+1 info scan.
		result = append(result, instance(record, orbstack.Info{Record: machine}))
	}
	return result, nil
}

func (p *Provider) DeleteInstance(ctx context.Context, target string) error {
	record, lock, err := p.locked(ctx, target, false)
	if errors.Is(err, gerrors.ErrNotFound) {
		machines, inventoryErr := p.inventory(ctx)
		if inventoryErr != nil {
			return inventoryErr
		}
		for _, machine := range machines {
			if machine.ID == target || machine.Name == target {
				return errors.New("refusing deletion of an unregistered existing machine")
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	return p.deleteRecord(ctx, record, false)
}

func (p *Provider) deleteRecord(ctx context.Context, record state.Record, explicitRelease bool) error {
	if record.MachineID == "" {
		return p.releaseReservation(ctx, record, explicitRelease)
	}
	if record.ControllerID != p.host.ControllerID || p.isTemplate(record.MachineID) {
		return errors.New("refusing deletion of an unowned machine or template")
	}
	record.Phase = state.PhaseDeleting
	if err := p.registry.Save(ctx, record); err != nil {
		return err
	}
	_, err := p.ownedInfo(ctx, record)
	if err != nil && !errors.Is(err, gerrors.ErrNotFound) {
		return err
	}
	if err == nil {
		if err := p.orb.Delete(ctx, record.MachineID); err != nil && !errors.Is(err, orbstack.ErrNotFound) {
			return err
		}
	}
	machines, err := p.inventory(ctx)
	if err != nil {
		return err
	}
	for _, machine := range machines {
		if machine.ID == record.MachineID {
			return errors.New("owned machine remains after delete")
		}
	}
	if err := p.registry.ClearCloneIntent(record.RunnerName); err != nil {
		return err
	}
	return p.registry.Remove(ctx, record.RunnerName)
}

func (p *Provider) releaseReservation(ctx context.Context, record state.Record, explicit bool) error {
	if record.Phase != state.PhaseReserved || record.MachineID != "" {
		return errors.New("only machine-less reservations can be released")
	}
	uncertain, err := p.registry.CloneUncertain(record.RunnerName)
	if err != nil {
		return err
	}
	if uncertain && !explicit {
		return errors.New("clone outcome is uncertain; inspect OrbStack and use explicit recover")
	}
	machines, err := p.inventory(ctx)
	if err != nil {
		return err
	}
	for _, machine := range machines {
		if machine.Name == record.MachineName {
			return errors.New("reserved machine name exists; inspect and explicitly adopt its ID")
		}
	}
	if err := p.registry.ClearCloneIntent(record.RunnerName); err != nil {
		return err
	}
	return p.registry.Remove(ctx, record.RunnerName)
}

func (p *Provider) RemoveAllInstances(ctx context.Context) error {
	records, err := p.registry.Snapshot(ctx)
	if err != nil {
		return err
	}
	var failures []error
	controllerScope := *p
	controllerScope.poolID = ""
	for _, record := range records {
		if err := controllerScope.DeleteInstance(ctx, record.RunnerName); err != nil {
			failures = append(failures, fmt.Errorf("deleting runner %q: %w", record.RunnerName, err))
		}
	}
	return errors.Join(failures...)
}

func (p *Provider) Start(ctx context.Context, target string) error {
	record, lock, err := p.locked(ctx, target, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if record.MachineID == "" {
		return gerrors.ErrNotFound
	}
	if _, err := p.ownedInfo(ctx, record); err != nil {
		return err
	}
	return p.orb.Start(ctx, record.MachineID)
}

// Stop deliberately ignores force: OrbStack has no documented targeted force
// flag. A normal targeted stop must fail rather than falling back to global stop.
func (p *Provider) Stop(ctx context.Context, target string, force bool) error {
	record, lock, err := p.locked(ctx, target, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	if record.MachineID == "" {
		return gerrors.ErrNotFound
	}
	if _, err := p.ownedInfo(ctx, record); err != nil {
		return err
	}
	return p.orb.Stop(ctx, record.MachineID)
}

// Recover explicitly adopts a pending clone after operator inspection, or
// releases its absent reservation. A held instance lock always refuses recovery.
func (p *Provider) Recover(ctx context.Context, runnerName, machineID string, release bool) error {
	if runnerName == "" || (machineID == "") == !release {
		return errors.New("recover requires runner-name and exactly one of machine-id or release")
	}
	record, lock, err := p.locked(ctx, runnerName, false)
	if err != nil {
		return err
	}
	defer lock.Close()
	if record.RunnerName != runnerName || record.Phase != state.PhaseReserved || record.MachineID != "" {
		return errors.New("recovery requires a machine-less pending reservation")
	}
	if release {
		return p.releaseReservation(ctx, record, true)
	}
	if p.isTemplate(machineID) {
		return errors.New("cannot adopt a registered template")
	}
	machines, err := p.inventory(ctx)
	if err != nil {
		return err
	}
	var observed *orbstack.Machine
	for i := range machines {
		if machines[i].ID == machineID {
			observed = &machines[i]
			break
		}
	}
	if observed == nil {
		return gerrors.ErrNotFound
	}
	if observed.Name != record.MachineName {
		return errors.New("adopted machine does not match the reserved name")
	}
	record.MachineID, record.Phase = machineID, state.PhaseCreated
	// Save checks global ID uniqueness, including other controllers.
	if err := p.registry.Save(ctx, record); err != nil {
		return err
	}
	return p.registry.ClearCloneIntent(runnerName)
}
