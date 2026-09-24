package state

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

var (
	ErrNotFound  = errors.New("registry record not found")
	ErrConflict  = errors.New("conflicting registry reservation")
	ErrCapacity  = errors.New("maximum runner capacity reserved")
	ErrBusy      = errors.New("instance operation is still in flight")
	errUnchanged = errors.New("registry unchanged")
)

// Registry is an initialized, controller-bound registry. Callers must hold the
// instance lock while mutating a runner; the global lock only protects short
// inventory transactions, never OrbStack operations or guest bootstrap.
type Registry struct {
	dir          string
	controllerID string
}

// Lock is released by closing the file, not LOCK_UN. A clone child inherits the
// same open file description and must keep the flock if its parent is killed.
type Lock struct{ File *os.File }

func (l *Lock) Close() error { return l.File.Close() }

func acquire(ctx context.Context, path string, create, shared, wait bool) (*Lock, error) {
	flags := os.O_RDWR | unix.O_NOFOLLOW
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0600)
	if err != nil {
		return nil, err
	}
	mode := unix.LOCK_EX | unix.LOCK_NB
	if shared {
		mode = unix.LOCK_SH | unix.LOCK_NB
	}
	for {
		if err = ctx.Err(); err != nil {
			file.Close()
			return nil, err
		}
		err = unix.Flock(int(file.Fd()), mode)
		if err == nil {
			return &Lock{File: file}, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EAGAIN) {
			file.Close()
			return nil, err
		}
		if !wait {
			file.Close()
			return nil, ErrBusy
		}
		select {
		case <-ctx.Done():
			file.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func writeAtomic(path string, value any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".registry-write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if err := json.NewEncoder(f).Encode(value); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// Initialize atomically publishes a complete empty layout. The private registry
// subdirectory may coexist with the controller database in stateDir. Existing
// records and persistent locks are never replaced or repaired.
func Initialize(ctx context.Context, stateDir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !filepath.IsAbs(stateDir) {
		return errors.New("registry state_dir must be absolute")
	}
	dir := filepath.Join(stateDir, "registry")
	if _, err := os.Lstat(dir); err == nil {
		_, err = Snapshot(ctx, stateDir, "")
		return err
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(stateDir, ".registry-init-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, name := range []string{"instances", "templates", "clone-intents"} {
		if err := os.Mkdir(filepath.Join(tmp, name), 0700); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(filepath.Join(tmp, "global.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := writeAtomic(filepath.Join(tmp, "records.json"), []Record{}); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another initializer may have won the atomic publication race.
		if _, checkErr := Snapshot(ctx, stateDir, ""); checkErr != nil {
			return fmt.Errorf("initializing registry: %w (existing layout: %v)", err, checkErr)
		}
		return nil
	}
	return syncDir(stateDir)
}

func checkLayout(dir string) error {
	for _, name := range []string{"", "instances", "templates", "clone-intents", "global.lock", "records.json"} {
		info, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("registry is not initialized: %w", err)
		}
		wantDir := name == "" || name == "instances" || name == "templates" || name == "clone-intents"
		if info.Mode()&os.ModeSymlink != 0 || info.IsDir() != wantDir || (!wantDir && !info.Mode().IsRegular()) {
			return fmt.Errorf("invalid registry layout entry %q", name)
		}
	}
	return nil
}

// Snapshot is a read-only, shared-locked read, including reservations. An empty
// controllerID is used internally to validate the complete initialized layout.
func Snapshot(ctx context.Context, stateDir, controllerID string) ([]Record, error) {
	dir := filepath.Join(stateDir, "registry")
	if err := checkLayout(dir); err != nil {
		return nil, err
	}
	lock, err := acquire(ctx, filepath.Join(dir, "global.lock"), false, true, true)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	records, err := readRecords(dir)
	if err != nil {
		return nil, err
	}
	result := records[:0]
	for _, record := range records {
		if controllerID == "" || record.ControllerID == controllerID {
			result = append(result, record)
		}
	}
	return result, nil
}

func readRecords(dir string) ([]Record, error) {
	f, err := os.OpenFile(filepath.Join(dir, "records.json"), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var records []Record
	if err := dec.Decode(&records); err != nil {
		return nil, fmt.Errorf("corrupt registry: %w", err)
	}
	if records == nil {
		return nil, errors.New("corrupt registry: expected an array, not null")
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return nil, errors.New("corrupt registry: trailing data")
	}
	if err := validateRecords(records); err != nil {
		return nil, err
	}
	return records, nil
}

func validateRecords(records []Record) error {
	keys, ids, names := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, r := range records {
		if !r.Valid() || strings.ContainsRune(r.ControllerID+r.RunnerName+r.MachineName+r.MachineID, 0) {
			return errors.New("corrupt registry: invalid record")
		}
		if _, err := uuid.Parse(r.ControllerID); err != nil {
			return errors.New("corrupt registry: invalid controller UUID")
		}
		switch r.Phase {
		case PhaseReserved:
			if r.MachineID != "" {
				return errors.New("corrupt registry: reservation has a machine ID")
			}
		case PhaseCreated, PhaseBootstrapping, PhaseActive, PhaseDeleting:
			if r.MachineID == "" {
				return errors.New("corrupt registry: machine-backed phase without ID")
			}
		default:
			return errors.New("corrupt registry: unknown phase")
		}
		key := recordKey(r.ControllerID, r.RunnerName)
		if keys[key] || names[r.MachineName] || (r.MachineID != "" && ids[r.MachineID]) {
			return errors.New("corrupt registry: duplicate ownership")
		}
		keys[key], names[r.MachineName] = true, true
		if r.MachineID != "" {
			ids[r.MachineID] = true
		}
	}
	return nil
}

func recordKey(controllerID, name string) string {
	sum := sha256.Sum256([]byte(controllerID + "\x00" + name))
	return hex.EncodeToString(sum[:])
}

// Open refuses missing or corrupt initialization; it never creates the layout.
func Open(ctx context.Context, stateDir, controllerID string) (*Registry, error) {
	if controllerID == "" {
		return nil, errors.New("registry requires controller identity")
	}
	if _, err := Snapshot(ctx, stateDir, controllerID); err != nil {
		return nil, err
	}
	return &Registry{dir: filepath.Join(stateDir, "registry"), controllerID: controllerID}, nil
}

func (r *Registry) Snapshot(ctx context.Context) ([]Record, error) {
	return Snapshot(ctx, filepath.Dir(r.dir), r.controllerID)
}

func (r *Registry) LockInstance(ctx context.Context, runnerName string) (*Lock, error) {
	return r.lockInstance(ctx, runnerName, true)
}

func (r *Registry) TryLockInstance(ctx context.Context, runnerName string) (*Lock, error) {
	return r.lockInstance(ctx, runnerName, false)
}

func (r *Registry) lockInstance(ctx context.Context, name string, wait bool) (*Lock, error) {
	if name == "" {
		return nil, errors.New("empty runner lock target")
	}
	return acquire(ctx, filepath.Join(r.dir, "instances", recordKey(r.controllerID, name)+".lock"), true, false, wait)
}

func (r *Registry) LockTemplate(ctx context.Context, machineID string) (*Lock, error) {
	if machineID == "" {
		return nil, errors.New("empty template lock target")
	}
	return acquire(ctx, filepath.Join(r.dir, "templates", recordKey("template", machineID)+".lock"), true, false, true)
}

// Find resolves only a recorded provider ID or original GARM runner name, never
// an OrbStack machine name. Provider IDs take precedence over runner names.
func (r *Registry) Find(ctx context.Context, target string) (Record, error) {
	if target == "" {
		return Record{}, ErrNotFound
	}
	records, err := r.Snapshot(ctx)
	if err != nil {
		return Record{}, err
	}
	for _, record := range records {
		if record.MachineID != "" && record.MachineID == target {
			return record, nil
		}
	}
	for _, record := range records {
		if record.RunnerName == target {
			return record, nil
		}
	}
	return Record{}, ErrNotFound
}

func (r *Registry) transaction(ctx context.Context, update func(*[]Record) error) error {
	lock, err := acquire(ctx, filepath.Join(r.dir, "global.lock"), false, false, true)
	if err != nil {
		return err
	}
	defer lock.Close()
	records, err := readRecords(r.dir)
	if err != nil {
		return err
	}
	if err := update(&records); err != nil {
		if err == errUnchanged {
			return nil
		}
		return err
	}
	if err := validateRecords(records); err != nil {
		return err
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].ControllerID == records[j].ControllerID {
			return records[i].RunnerName < records[j].RunnerName
		}
		return records[i].ControllerID < records[j].ControllerID
	})
	return writeAtomic(filepath.Join(r.dir, "records.json"), records)
}

// Reserve counts all of this controller's records, including machine-less
// reservations, in the same transaction that claims a capacity slot.
func (r *Registry) Reserve(ctx context.Context, desired Record, max int) (Record, bool, error) {
	if desired.ControllerID != r.controllerID || desired.Phase != PhaseReserved || max < 1 {
		return Record{}, false, ErrConflict
	}
	var existing Record
	found := false
	err := r.transaction(ctx, func(records *[]Record) error {
		count := 0
		for _, record := range *records {
			if record.ControllerID != r.controllerID {
				continue
			}
			count++
			if record.RunnerName == desired.RunnerName {
				if record.PoolID != desired.PoolID || record.ImageID != desired.ImageID || record.Flavor != desired.Flavor {
					return ErrConflict
				}
				existing, found = record, true
				return errUnchanged
			}
		}
		if count >= max {
			return ErrCapacity
		}
		*records = append(*records, desired)
		existing = desired
		return nil
	})
	return existing, !found, err
}

// Save updates an existing reservation without changing its ownership key.
// The caller holds its instance lock. Uniqueness spans every controller.
func (r *Registry) Save(ctx context.Context, record Record) error {
	if record.ControllerID != r.controllerID {
		return ErrConflict
	}
	return r.transaction(ctx, func(records *[]Record) error {
		for i, old := range *records {
			if old.ControllerID != r.controllerID || old.RunnerName != record.RunnerName {
				continue
			}
			if old.PoolID != record.PoolID || old.ImageID != record.ImageID || old.Flavor != record.Flavor || old.MachineName != record.MachineName || (old.MachineID != "" && old.MachineID != record.MachineID) {
				return ErrConflict
			}
			if old == record {
				return errUnchanged
			}
			switch old.Phase {
			case PhaseReserved:
				if record.Phase != PhaseCreated {
					return ErrConflict
				}
			case PhaseCreated:
				if record.Phase != PhaseBootstrapping && record.Phase != PhaseDeleting {
					return ErrConflict
				}
			case PhaseBootstrapping:
				if record.Phase != PhaseActive && record.Phase != PhaseDeleting {
					return ErrConflict
				}
			case PhaseActive:
				if record.Phase != PhaseDeleting {
					return ErrConflict
				}
			default:
				return ErrConflict
			}
			(*records)[i] = record
			return nil
		}
		return ErrNotFound
	})
}

func (r *Registry) Remove(ctx context.Context, runnerName string) error {
	return r.transaction(ctx, func(records *[]Record) error {
		for i, old := range *records {
			if old.ControllerID == r.controllerID && old.RunnerName == runnerName {
				*records = append((*records)[:i], (*records)[i+1:]...)
				return nil
			}
		}
		return errUnchanged
	})
}

// MarkCloneIntent is durable before spawning orbctl. After an interrupted clone
// with no recorded ID, automatic deletion cannot infer backend completion from
// a momentarily empty inventory. Only explicit operator recovery resolves it.
func (r *Registry) MarkCloneIntent(runnerName string) error {
	return writeAtomic(r.intentPath(runnerName), struct {
		Started bool `json:"started"`
	}{true})
}

func (r *Registry) CloneUncertain(runnerName string) (bool, error) {
	_, err := os.Lstat(r.intentPath(runnerName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

func (r *Registry) ClearCloneIntent(runnerName string) error {
	if err := os.Remove(r.intentPath(runnerName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Join(r.dir, "clone-intents"))
}

func (r *Registry) intentPath(runnerName string) string {
	return filepath.Join(r.dir, "clone-intents", recordKey(r.controllerID, runnerName)+".json")
}
