// Package templates builds, seals and describes immutable, credential-free
// runner template machines and their host-side manifests.
//
// types.go is the frozen cross-slice read model; the builder and manifest
// reader live in this package as well.
package templates

// SchemaVersion is the template manifest schema.
const SchemaVersion = 1

// Manifest is the host-side description of a sealed template machine. It is
// written next to the registry when a template is built and registered, and
// the same nonsecret metadata is recorded inside the template machine.
type Manifest struct {
	SchemaVersion int    `json:"schema_version"`
	ImageID       string `json:"image_id"`
	MachineID     string `json:"machine_id"`
	// OSVersion is the version label OrbStack itself records for the
	// template machine (for example "noble" for Ubuntu 24.04). Template
	// validation compares against this observed value, never a hardcoded
	// marketing version.
	OSVersion       string            `json:"os_version"`
	RecipeSHA256    string            `json:"recipe_sha256"`
	Arch            string            `json:"arch"`
	RunnerFilename  string            `json:"runner_filename"`
	RunnerSHA256    string            `json:"runner_sha256"`
	OrbStackVersion string            `json:"orbstack_version"`
	Packages        map[string]string `json:"packages"`
}
