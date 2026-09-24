package state

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const testController = "33145d47-d0ce-40f4-8e28-397b5dcf891c"

func reservation(name string) Record {
	return Record{SchemaVersion: SchemaVersion, ControllerID: testController, PoolID: "scale-set-opaque", RunnerName: name, MachineName: "machine-" + name, ImageID: "ubuntu-arm64-v1", Flavor: "default", Phase: PhaseReserved}
}

func TestSnapshotRequiresInitializationAndPreservesNativeState(t *testing.T) {
	dir := t.TempDir()
	if _, err := Snapshot(context.Background(), dir, testController); err == nil {
		t.Fatal("missing initialization accepted")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("Snapshot mutated directory: %v %v", entries, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "garm.db"), []byte("controller state"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	records, err := Snapshot(context.Background(), dir, testController)
	if err != nil || records == nil || len(records) != 0 {
		t.Fatalf("first install snapshot = %#v, %v", records, err)
	}
	reg, err := Open(context.Background(), dir, testController)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := reg.Reserve(context.Background(), reservation("one"), 2); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	records, err = Snapshot(context.Background(), dir, testController)
	if err != nil || len(records) != 1 || records[0].RunnerName != "one" {
		t.Fatalf("reinitialization lost reservation: %#v, %v", records, err)
	}
	db, err := os.ReadFile(filepath.Join(dir, "garm.db"))
	if err != nil || string(db) != "controller state" {
		t.Fatalf("native DB modified: %q, %v", db, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "registry", "records.json"), []byte("null\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Initialize(context.Background(), dir); err == nil {
		t.Fatal("corrupt layout repaired silently")
	}
	if _, err := Snapshot(context.Background(), dir, testController); err == nil {
		t.Fatal("corrupt state became an empty inventory")
	}
}

func TestReservationsEnforceOwnershipAndCapacity(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := Initialize(ctx, dir); err != nil {
		t.Fatal(err)
	}
	reg, err := Open(ctx, dir, testController)
	if err != nil {
		t.Fatal(err)
	}
	record, fresh, err := reg.Reserve(ctx, reservation("one"), 1)
	if err != nil || !fresh {
		t.Fatalf("reserve: %v, %v", fresh, err)
	}
	again, fresh, err := reg.Reserve(ctx, reservation("one"), 1)
	if err != nil || fresh || again != record {
		t.Fatalf("idempotency: %#v %v %v", again, fresh, err)
	}
	conflict := reservation("one")
	conflict.PoolID = "other-pool"
	if _, _, err := reg.Reserve(ctx, conflict, 1); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting pool accepted: %v", err)
	}
	if _, _, err := reg.Reserve(ctx, reservation("two"), 1); !errors.Is(err, ErrCapacity) {
		t.Fatalf("reservations did not consume capacity: %v", err)
	}
	record.MachineID, record.Phase = "owned-id", PhaseCreated
	if err := reg.Save(ctx, record); err != nil {
		t.Fatal(err)
	}
	foreign, err := Open(ctx, dir, "8e91cab5-2f88-4d58-878d-a0895a07fe62")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := foreign.Find(ctx, "owned-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("foreign controller resolved ownership: %v", err)
	}
	if err := foreign.Save(ctx, record); !errors.Is(err, ErrConflict) {
		t.Fatalf("foreign controller mutated record: %v", err)
	}
	if _, err := reg.Find(ctx, record.MachineName); !errors.Is(err, ErrNotFound) {
		t.Fatal("OrbStack machine name was treated as ownership")
	}
}

func TestRegistrySubprocess(t *testing.T) {
	mode := os.Getenv("GARM_REGISTRY_TEST_MODE")
	if mode == "" {
		return
	}
	if mode == "hold" {
		fmt.Print("ready\n")
		_, err := io.Copy(io.Discard, os.Stdin)
		if err != nil {
			os.Exit(10)
		}
		os.Exit(0)
	}
	ctx := context.Background()
	reg, err := Open(ctx, os.Getenv("GARM_REGISTRY_TEST_DIR"), testController)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(10)
	}
	name := os.Getenv("GARM_REGISTRY_TEST_NAME")
	lock, err := reg.LockInstance(ctx, name)
	if err != nil {
		os.Exit(11)
	}
	_, _, err = reg.Reserve(ctx, reservation(name), 2)
	lock.Close()
	if errors.Is(err, ErrCapacity) {
		os.Exit(42)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(12)
	}
	os.Exit(0)
}

func TestCapacityIsAtomicAcrossProcesses(t *testing.T) {
	dir := t.TempDir()
	if err := Initialize(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 8)
	for i := range commands {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRegistrySubprocess$")
		cmd.Env = append(os.Environ(), "GARM_REGISTRY_TEST_MODE=reserve", "GARM_REGISTRY_TEST_DIR="+dir, "GARM_REGISTRY_TEST_NAME="+strconv.Itoa(i))
		commands[i] = cmd
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
	}
	successes := 0
	for _, cmd := range commands {
		err := cmd.Wait()
		if err == nil {
			successes++
			continue
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 42 {
			t.Fatalf("unexpected reservation process error: %v", err)
		}
	}
	if successes != 2 {
		t.Fatalf("%d processes reserved capacity, wanted exactly two", successes)
	}
	records, err := Snapshot(context.Background(), dir, testController)
	if err != nil || len(records) != 2 {
		t.Fatalf("lost durable reservations: %#v %v", records, err)
	}
}

func TestInheritedLockSurvivesParentClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := Initialize(ctx, dir); err != nil {
		t.Fatal(err)
	}
	reg, err := Open(ctx, dir, testController)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := reg.LockInstance(ctx, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	cmd := exec.Command(os.Args[0], "-test.run=^TestRegistrySubprocess$")
	cmd.Env = append(os.Environ(), "GARM_REGISTRY_TEST_MODE=hold")
	cmd.ExtraFiles = []*os.File{lock.File}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { stdin.Close(); cmd.Process.Kill(); cmd.Wait() }()
	ready := make([]byte, len("ready\n"))
	if _, err := io.ReadFull(stdout, ready); err != nil || !strings.HasPrefix(string(ready), "ready") {
		t.Fatalf("child handshake: %q %v", ready, err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if other, err := reg.TryLockInstance(ctx, "orphan"); !errors.Is(err, ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatalf("live child did not retain instance lock: %v", err)
	}
	stdin.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	other, err := reg.TryLockInstance(ctx, "orphan")
	if err != nil {
		t.Fatalf("child exit did not release lock: %v", err)
	}
	other.Close()
}

func TestTransitionsAndMachineOwnershipAreAtomic(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := Initialize(ctx, dir); err != nil {
		t.Fatal(err)
	}
	reg, err := Open(ctx, dir, testController)
	if err != nil {
		t.Fatal(err)
	}
	one, _, err := reg.Reserve(ctx, reservation("one"), 2)
	if err != nil {
		t.Fatal(err)
	}
	two, _, err := reg.Reserve(ctx, reservation("two"), 2)
	if err != nil {
		t.Fatal(err)
	}
	one.MachineID, one.Phase = "unique-id", PhaseCreated
	if err := reg.Save(ctx, one); err != nil {
		t.Fatal(err)
	}
	two.MachineID, two.Phase = "unique-id", PhaseCreated
	if err := reg.Save(ctx, two); err == nil {
		t.Fatal("same machine assigned to two reservations")
	}
	stillReserved, err := reg.Find(ctx, "two")
	if err != nil || stillReserved.MachineID != "" || stillReserved.Phase != PhaseReserved {
		t.Fatalf("failed ownership change partially committed: %#v %v", stillReserved, err)
	}
	one.Phase = PhaseActive
	if err := reg.Save(ctx, one); !errors.Is(err, ErrConflict) {
		t.Fatalf("skipped bootstrap transition: %v", err)
	}
	one.Phase = PhaseBootstrapping
	if err := reg.Save(ctx, one); err != nil {
		t.Fatal(err)
	}
	one.Phase = PhaseActive
	if err := reg.Save(ctx, one); err != nil {
		t.Fatal(err)
	}
	one.Phase = PhaseCreated
	if err := reg.Save(ctx, one); !errors.Is(err, ErrConflict) {
		t.Fatalf("active runner returned to prebootstrap phase: %v", err)
	}
}
