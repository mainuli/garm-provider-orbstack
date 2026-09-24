package provider

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	gerrors "github.com/cloudbase/garm-provider-common/errors"
	"github.com/cloudbase/garm-provider-common/params"
	"github.com/mainuli/garm-provider-orbstack/internal/config"
	"github.com/mainuli/garm-provider-orbstack/internal/orbstack"
	"github.com/mainuli/garm-provider-orbstack/internal/state"
	"github.com/mainuli/garm-provider-orbstack/internal/templates"
)

const controllerID = "33145d47-d0ce-40f4-8e28-397b5dcf891c"

type memoryMachines struct {
	machines                             map[string]orbstack.Machine
	settings                             map[string]bool
	clones, enqueued                     int
	deleted, started, stopped            []string
	inventoryErr, settingsErr, deleteErr error
	cloneFailure                         bool
	beforeClone                          func(string)
	payload                              []byte
}

func (m *memoryMachines) CloneCmd(ctx context.Context, source, dest string) *exec.Cmd {
	m.clones++
	if m.beforeClone != nil {
		m.beforeClone(dest)
	}
	if m.cloneFailure {
		return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 9")
	}
	template := m.machines[source]
	template.ID, template.Name, template.State = "runner-id", dest, orbstack.StateStopped
	m.machines[template.ID] = template
	return exec.CommandContext(ctx, "/bin/sh", "-c", "exit 0")
}
func (m *memoryMachines) Info(_ context.Context, target string) (orbstack.Info, error) {
	for _, machine := range m.machines {
		if machine.ID == target || machine.Name == target {
			return orbstack.Info{Record: machine, IP4: "192.0.2.10"}, nil
		}
	}
	return orbstack.Info{}, orbstack.ErrNotFound
}
func (m *memoryMachines) List(context.Context) ([]orbstack.Machine, error) {
	if m.inventoryErr != nil {
		return nil, m.inventoryErr
	}
	machines := make([]orbstack.Machine, 0, len(m.machines))
	for _, machine := range m.machines {
		machines = append(machines, machine)
	}
	return machines, nil
}
func (m *memoryMachines) ApplySettings(_ context.Context, name string, settings orbstack.MachineSettings) error {
	if m.settingsErr != nil {
		return m.settingsErr
	}
	for _, machine := range m.machines {
		if machine.Name != name {
			continue
		}
		if machine.State != orbstack.StateStopped || !settings.Isolated || settings.ForwardSSHAgent || settings.IsolateNetwork || settings.CPUs != 2 || settings.MemoryMiB != 4096 || settings.DiskBytes != 68719476736 {
			return errors.New("unsafe settings or settings applied after boot")
		}
		m.settings[machine.ID] = true
		return nil
	}
	return orbstack.ErrNotFound
}
func (m *memoryMachines) Start(_ context.Context, id string) error {
	machine, ok := m.machines[id]
	if !ok || id == "" || !m.settings[id] {
		return errors.New("start without owned ID or preboot settings")
	}
	machine.State = orbstack.StateRunning
	m.machines[id] = machine
	m.started = append(m.started, id)
	return nil
}
func (m *memoryMachines) Stop(_ context.Context, id string) error {
	machine, ok := m.machines[id]
	if !ok || id == "" {
		return errors.New("stop without owned ID")
	}
	machine.State = orbstack.StateStopped
	m.machines[id] = machine
	m.stopped = append(m.stopped, id)
	return nil
}
func (m *memoryMachines) Delete(_ context.Context, id string) error {
	if m.deleteErr != nil {
		return m.deleteErr
	}
	if id == "" || id == "template-id" || id == "foreign-id" {
		return errors.New("foreign destructive target")
	}
	m.deleted = append(m.deleted, id)
	delete(m.machines, id)
	return nil
}
func (m *memoryMachines) Run(_ context.Context, options orbstack.RunOptions) error {
	if options.Machine != "runner-id" || options.User != "root" {
		return errors.New("incorrect guest target")
	}
	if options.Stdin != nil {
		data, err := io.ReadAll(options.Stdin)
		if err != nil {
			return err
		}
		m.payload = append(m.payload, data...)
	}
	if reflect.DeepEqual(options.Command, []string{"systemctl", "start", "--no-block", "garm-bootstrap.service"}) {
		m.enqueued++
	}
	return nil
}

func fixture(t *testing.T) (*Provider, *memoryMachines, params.BootstrapInstance) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := state.Initialize(ctx, dir); err != nil {
		t.Fatal(err)
	}
	manifest := templates.Manifest{SchemaVersion: 1, ImageID: "ubuntu-arm64-v1", MachineID: "template-id", OSVersion: "noble", RecipeSHA256: strings.Repeat("a", 64), RunnerSHA256: strings.Repeat("b", 64), Arch: "arm64", RunnerFilename: "actions-runner-linux-arm64-2.333.0.tar.gz", OrbStackVersion: "2.2.3", Packages: map[string]string{"git": "1:2.43.0"}}
	manifestPath := filepath.Join(dir, "template.json")
	if err := templates.WriteManifest(manifestPath, manifest); err != nil {
		t.Fatal(err)
	}
	host := config.Host{ControllerID: controllerID, StateDir: dir, OrbctlPath: "/unused/orbctl", OperationTimeout: time.Second, MaxInstances: 2,
		Images:  map[string]config.Image{"ubuntu-arm64-v1": {MachineID: "template-id", ManifestPath: manifestPath, Arch: "arm64"}},
		Flavors: map[string]config.Flavor{"default": {CPUs: 2, MemoryMiB: 4096, DiskBytes: 68719476736}}}
	p, err := New(ctx, host, controllerID, "scale-set-entity-opaque")
	if err != nil {
		t.Fatal(err)
	}
	backend := &memoryMachines{settings: map[string]bool{}, machines: map[string]orbstack.Machine{
		"template-id": {ID: "template-id", Name: "approved-template", State: orbstack.StateStopped, Image: orbstack.Image{Distro: "ubuntu", Version: "noble", Arch: "arm64"}, Config: orbstack.MachineConfig{Isolated: true}},
		"foreign-id":  {ID: "foreign-id", Name: "foreign-name", State: orbstack.StateRunning},
	}}
	p.orb = backend
	bootstrap := params.BootstrapInstance{Name: "garm-runner-one", PoolID: p.poolID, OSType: params.Linux, OSArch: params.Arm64, Image: "ubuntu-arm64-v1", Flavor: "default", InstanceToken: "secret-instance-token", Tools: []params.RunnerApplicationDownload{{OS: new("linux"), Architecture: new("arm64"), Filename: new(manifest.RunnerFilename), DownloadURL: new("https://github.com/actions/runner/releases/download/v2.333.0/" + manifest.RunnerFilename), SHA256Checksum: new(manifest.RunnerSHA256)}}}
	return p, backend, bootstrap
}

func TestCreateIdempotencyAndOwnedDelete(t *testing.T) {
	ctx := context.Background()
	p, machines, bootstrap := fixture(t)
	machines.beforeClone = func(name string) {
		record, err := p.registry.Find(ctx, bootstrap.Name)
		if err != nil || record.Phase != state.PhaseReserved || record.MachineName != name || record.MachineID != "" {
			t.Fatalf("clone preceded reservation: %#v %v", record, err)
		}
	}
	first, err := p.CreateInstance(ctx, bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	if first.ProviderID != "runner-id" || first.Name != bootstrap.Name || first.Status != params.InstanceRunning || first.OSArch != params.Arm64 || first.OSVersion != "noble" {
		t.Fatalf("wrong real-machine result: %#v", first)
	}
	bootstrap.InstanceToken = "different-retry-token"
	second, err := p.CreateInstance(ctx, bootstrap)
	if err != nil || !reflect.DeepEqual(first, second) || machines.clones != 1 || machines.enqueued != 1 {
		t.Fatalf("retry created or bootstrapped again: %#v %v, clones=%d, enqueued=%d", second, err, machines.clones, machines.enqueued)
	}
	if strings.Contains(string(machines.payload), "different-retry-token") {
		t.Fatal("retry overwrote credentials")
	}
	record, err := p.registry.Find(ctx, bootstrap.Name)
	if err != nil || record.Phase != state.PhaseActive || record.PoolID != p.poolID {
		t.Fatalf("active ownership not durable: %#v %v", record, err)
	}
	if err := p.DeleteInstance(ctx, first.ProviderID); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteInstance(ctx, first.ProviderID); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(machines.deleted, []string{"runner-id"}) {
		t.Fatalf("delete did not target the recorded ID exactly once: %v", machines.deleted)
	}
	if _, exists := machines.machines["foreign-id"]; !exists {
		t.Fatal("foreign machine deleted")
	}
	if _, exists := machines.machines["template-id"]; !exists {
		t.Fatal("template deleted")
	}
}

func TestForeignControllerPoolAndTargetsCannotMutate(t *testing.T) {
	ctx := context.Background()
	p, machines, bootstrap := fixture(t)
	if _, err := New(ctx, p.host, "6ee8de47-cd6e-4bba-ac4d-a6f4c729467e", ""); err == nil {
		t.Fatal("foreign controller authorized")
	}
	for _, target := range []string{"", "foreign-id", "foreign-name", "template-id", "approved-template"} {
		if err := p.DeleteInstance(ctx, target); err == nil {
			t.Fatalf("delete accepted foreign/empty target %q", target)
		}
		if err := p.Start(ctx, target); err == nil {
			t.Fatalf("start accepted foreign/empty target %q", target)
		}
		if err := p.Stop(ctx, target, true); err == nil {
			t.Fatalf("stop accepted foreign/empty target %q", target)
		}
	}
	wrongPool := bootstrap
	wrongPool.PoolID = "other-scale-set"
	if _, err := p.CreateInstance(ctx, wrongPool); err == nil {
		t.Fatal("pool mismatch accepted")
	}
	if machines.clones != 0 || len(machines.deleted) != 0 {
		t.Fatal("authorization failure mutated OrbStack")
	}
	if _, err := p.CreateInstance(ctx, bootstrap); err != nil {
		t.Fatal(err)
	}
	other := *p
	other.poolID = "other-scale-set"
	if err := other.DeleteInstance(ctx, "runner-id"); err == nil {
		t.Fatal("foreign pool deleted owned runner")
	}
	bootstrap.PoolID, other.poolID = "other-scale-set", "other-scale-set"
	if _, err := other.CreateInstance(ctx, bootstrap); !errors.Is(err, gerrors.ErrDuplicateEntity) {
		t.Fatalf("conflicting reservation not duplicate: %v", err)
	}
}

func TestFailedBootstrapPreparationRetainsFailedCleanup(t *testing.T) {
	ctx := context.Background()
	p, machines, bootstrap := fixture(t)
	machines.settingsErr, machines.deleteErr = errors.New("settings unavailable"), errors.New("delete unavailable")
	if _, err := p.CreateInstance(ctx, bootstrap); err == nil {
		t.Fatal("failed settings reported success")
	}
	record, err := p.registry.Find(ctx, bootstrap.Name)
	if err != nil || record.MachineID != "runner-id" || record.Phase != state.PhaseDeleting {
		t.Fatalf("failed cleanup lost machine ownership: %#v %v", record, err)
	}
	if machines.enqueued != 0 || len(machines.started) != 0 {
		t.Fatal("booted without complete settings")
	}
	machines.deleteErr = nil
	if err := p.DeleteInstance(ctx, bootstrap.Name); err != nil {
		t.Fatal(err)
	}
}

func TestReservationsRequireSafeExplicitRecovery(t *testing.T) {
	ctx := context.Background()
	p, machines, bootstrap := fixture(t)
	machines.cloneFailure = true
	if _, err := p.CreateInstance(ctx, bootstrap); err == nil {
		t.Fatal("failed clone reported success")
	}
	record, err := p.registry.Find(ctx, bootstrap.Name)
	if err != nil || record.Phase != state.PhaseReserved || record.MachineID != "" {
		t.Fatalf("crash window was not retained: %#v %v", record, err)
	}
	instances, err := p.ListInstances(ctx, p.poolID)
	if err != nil || instances == nil || len(instances) != 0 {
		t.Fatalf("reservation escaped into provider instances: %#v %v", instances, err)
	}
	if err := p.DeleteInstance(ctx, bootstrap.Name); err == nil {
		t.Fatal("uncertain clone automatically released on absence")
	}
	lock, err := p.registry.LockInstance(ctx, bootstrap.Name)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(ctx, bootstrap.Name, "", true); !errors.Is(err, state.ErrBusy) {
		t.Fatalf("released live clone: %v", err)
	}
	lock.Close()
	machines.inventoryErr = errors.New("inventory unavailable")
	if err := p.Recover(ctx, bootstrap.Name, "", true); err == nil {
		t.Fatal("failed inventory released reservation")
	}
	if _, err := p.ListInstances(ctx, p.poolID); err == nil {
		t.Fatal("inventory failure became empty list")
	}
	machines.inventoryErr = nil
	machines.machines["orphan-id"] = orbstack.Machine{ID: "orphan-id", Name: record.MachineName, State: orbstack.StateStopped}
	if err := p.Recover(ctx, bootstrap.Name, "", true); err == nil {
		t.Fatal("released a matching orphan")
	}
	if err := p.Recover(ctx, bootstrap.Name, "template-id", false); err == nil {
		t.Fatal("adopted registered template")
	}
	if err := p.Recover(ctx, bootstrap.Name, "foreign-id", false); err == nil {
		t.Fatal("adopted unrelated name")
	}
	if err := p.Recover(ctx, bootstrap.Name, "orphan-id", false); err != nil {
		t.Fatal(err)
	}
	if err := p.Recover(ctx, bootstrap.Name, "orphan-id", false); err == nil {
		t.Fatal("re-adopted machine-backed record")
	}
	if err := p.DeleteInstance(ctx, bootstrap.Name); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(machines.deleted, []string{"orphan-id"}) {
		t.Fatalf("wrong adopted target deleted: %v", machines.deleted)
	}
}

func TestPrecloneReleaseAndNameReuseDoNotAdoptForeignMachine(t *testing.T) {
	ctx := context.Background()
	p, machines, bootstrap := fixture(t)
	record := state.Record{SchemaVersion: state.SchemaVersion, ControllerID: controllerID, PoolID: p.poolID, RunnerName: bootstrap.Name, MachineName: "reserved-orb-name", ImageID: bootstrap.Image, Flavor: bootstrap.Flavor, Phase: state.PhaseReserved}
	if _, _, err := p.registry.Reserve(ctx, record, 2); err != nil {
		t.Fatal(err)
	}
	if err := p.DeleteInstance(ctx, bootstrap.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := p.registry.Find(ctx, bootstrap.Name); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("preclone reservation not reclaimed: %v", err)
	}
	if _, err := p.CreateInstance(ctx, bootstrap); err != nil {
		t.Fatal(err)
	}
	old, err := p.registry.Find(ctx, bootstrap.Name)
	if err != nil {
		t.Fatal(err)
	}
	delete(machines.machines, "runner-id")
	machines.machines["replacement-id"] = orbstack.Machine{ID: "replacement-id", Name: old.MachineName, State: orbstack.StateRunning}
	if err := p.DeleteInstance(ctx, "runner-id"); err != nil {
		t.Fatal(err)
	}
	if _, ok := machines.machines["replacement-id"]; !ok || len(machines.deleted) != 0 {
		t.Fatal("name reuse deleted a foreign replacement")
	}
}

func TestSupportedStatusMapping(t *testing.T) {
	for observed, wanted := range map[string]params.InstanceStatus{"running": params.InstanceRunning, "stopped": params.InstanceStopped, "failed": params.InstanceError, "starting": params.InstanceStatusUnknown} {
		got := instance(state.Record{MachineID: "id", RunnerName: "runner"}, orbstack.Info{Record: orbstack.Machine{State: observed}})
		if got.Status != wanted {
			t.Fatalf("status %q became %q, wanted %q", observed, got.Status, wanted)
		}
	}
}

func TestExplicitAbsentReleaseReturnsCapacity(t *testing.T) {
	p, machines, bootstrap := fixture(t)
	machines.cloneFailure = true
	if _, err := p.CreateInstance(t.Context(), bootstrap); err == nil {
		t.Fatal("failed clone reported success")
	}
	if err := p.Recover(t.Context(), bootstrap.Name, "", true); err != nil {
		t.Fatal(err)
	}
	records, err := p.registry.Snapshot(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("explicit release did not return capacity: %#v %v", records, err)
	}
	machines.cloneFailure = false
	if _, err := p.CreateInstance(t.Context(), bootstrap); err != nil {
		t.Fatalf("released runner name could not be retried: %v", err)
	}
}

func TestStartStopAndControllerWideRemoval(t *testing.T) {
	p, machines, bootstrap := fixture(t)
	if _, err := p.CreateInstance(t.Context(), bootstrap); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop(t.Context(), bootstrap.Name, true); err != nil {
		t.Fatal(err)
	}
	stopped, err := p.GetInstance(t.Context(), "runner-id")
	if err != nil || stopped.Status != params.InstanceStopped {
		t.Fatalf("targeted stop not observed: %#v %v", stopped, err)
	}
	if err := p.Start(t.Context(), "runner-id"); err != nil {
		t.Fatal(err)
	}
	started, err := p.GetInstance(t.Context(), bootstrap.Name)
	if err != nil || started.Status != params.InstanceRunning {
		t.Fatalf("targeted start not observed: %#v %v", started, err)
	}
	otherPool := state.Record{SchemaVersion: 1, ControllerID: controllerID, RunnerName: "pending-other-pool", MachineName: "reserved-other-pool", PoolID: "other-pool", ImageID: bootstrap.Image, Flavor: bootstrap.Flavor, Phase: state.PhaseReserved}
	if _, _, err := p.registry.Reserve(t.Context(), otherPool, 2); err != nil {
		t.Fatal(err)
	}
	if err := p.RemoveAllInstances(t.Context()); err != nil {
		t.Fatal(err)
	}
	records, err := p.registry.Snapshot(t.Context())
	if err != nil || len(records) != 0 {
		t.Fatalf("controller cleanup left runner records: %#v %v", records, err)
	}
	if len(machines.machines) != 2 || machines.machines["template-id"].ID != "template-id" || machines.machines["foreign-id"].ID != "foreign-id" {
		t.Fatalf("controller cleanup changed foreign/template machines: %#v", machines.machines)
	}
}

func TestListFiltersPoolsReservationsAndMissingMachines(t *testing.T) {
	p, machines, bootstrap := fixture(t)
	if _, err := p.CreateInstance(t.Context(), bootstrap); err != nil {
		t.Fatal(err)
	}
	pending := state.Record{SchemaVersion: 1, ControllerID: controllerID, RunnerName: "uncloned", MachineName: "uncloned-name", PoolID: p.poolID, ImageID: bootstrap.Image, Flavor: bootstrap.Flavor, Phase: state.PhaseReserved}
	if _, _, err := p.registry.Reserve(t.Context(), pending, 3); err != nil {
		t.Fatal(err)
	}
	otherPool := pending
	otherPool.RunnerName, otherPool.MachineName, otherPool.PoolID = "other-pool-runner", "other-pool-machine", "other-pool"
	if _, _, err := p.registry.Reserve(t.Context(), otherPool, 3); err != nil {
		t.Fatal(err)
	}
	otherPool.MachineID, otherPool.Phase = "other-pool-id", state.PhaseCreated
	if err := p.registry.Save(t.Context(), otherPool); err != nil {
		t.Fatal(err)
	}
	machines.machines[otherPool.MachineID] = orbstack.Machine{ID: otherPool.MachineID, Name: otherPool.MachineName, State: orbstack.StateRunning}
	listed, err := p.ListInstances(t.Context(), p.poolID)
	if err != nil || len(listed) != 1 || listed[0].ProviderID != "runner-id" || listed[0].Name != bootstrap.Name {
		t.Fatalf("list leaked another pool or reservation: %#v %v", listed, err)
	}
	delete(machines.machines, "runner-id")
	listed, err = p.ListInstances(t.Context(), p.poolID)
	if err != nil || listed == nil || len(listed) != 0 {
		t.Fatalf("missing machine became a provider instance: %#v %v", listed, err)
	}
	if _, err := p.GetInstance(t.Context(), bootstrap.Name); !errors.Is(err, gerrors.ErrNotFound) {
		t.Fatalf("missing owned machine not mapped to SDK not-found: %v", err)
	}
}
