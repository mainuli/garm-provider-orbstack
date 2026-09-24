// Package state owns the operator-side runner registry: the durable records
// that bind GARM runner names to OrbStack machines for one controller.
//
// types.go is the frozen cross-slice read model; registry.go implements the
// registry layout, locking and persistence.
package state

// SchemaVersion is the registry record schema. It must never silently
// change for existing installations; incompatible changes bump it with a
// migration path.
const SchemaVersion = 1

// Record lifecycle phases. Phases are internal registry states and are never
// emitted to GARM as provider statuses.
const (
	// PhaseReserved is set before the template clone starts. The machine
	// name is unique and capacity is counted.
	PhaseReserved = "reserved"
	// PhaseCreated is set once the OrbStack machine ID is durably recorded.
	PhaseCreated = "created"
	// PhaseBootstrapping is set before enqueueing the guest bootstrap unit.
	PhaseBootstrapping = "bootstrapping"
	// PhaseActive means provider creation completed; it does not mean
	// GitHub accepted a job.
	PhaseActive = "active"
	// PhaseDeleting is set while deletion is in progress, until absence is
	// confirmed.
	PhaseDeleting = "deleting"
)

// Record is one registry entry, keyed by (ControllerID, RunnerName).
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	ControllerID  string `json:"controller_id"`
	PoolID        string `json:"pool_id"`
	RunnerName    string `json:"runner_name"`
	MachineName   string `json:"machine_name"`
	MachineID     string `json:"machine_id"`
	ImageID       string `json:"image_id"`
	Flavor        string `json:"flavor"`
	Phase         string `json:"phase"`
}

// Valid reports whether the record is structurally complete and carries the
// expected schema version.
func (r Record) Valid() bool {
	return r.SchemaVersion == SchemaVersion &&
		r.ControllerID != "" &&
		r.RunnerName != "" &&
		r.MachineName != "" &&
		r.PoolID != "" &&
		r.ImageID != "" &&
		r.Flavor != "" &&
		r.Phase != ""
}
