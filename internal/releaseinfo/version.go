// Package releaseinfo carries release-build metadata injected via Go linker
// -X flags. Development builds keep the zero values; a development binary is
// not an installable release.
package releaseinfo

var (
	// Version is the release tag (for example v0.1.0), "dev" in development
	// builds.
	Version = "dev"
	// GARMImage is the digest-pinned upstream GARM image reference the
	// controller image is derived from, for example
	// ghcr.io/cloudbase/garm@sha256:... . Empty in development builds.
	GARMImage = ""
	// ControllerImage is the digest reference of the published derived
	// controller image, for example
	// ghcr.io/mainuli/garm-provider-orbstack@sha256:... . Empty in
	// development builds.
	ControllerImage = ""
)
