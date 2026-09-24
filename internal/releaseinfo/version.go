// Package releaseinfo carries release-build metadata injected via Go linker
// -X flags. Development builds keep the zero values; a development binary is
// not an installable release.
package releaseinfo

var (
	// Version is the release tag (for example v0.1.0), "dev" in development
	// builds.
	Version = "dev"
	// GARMSource identifies the exact upstream GARM revision the bundled
	// garm/garm-cli binaries were built from, for example
	// "cloudbase/garm v0.2.1 (154638445c3949c1958b01812f69d9a1e4d82684)".
	// Empty in development builds.
	GARMSource = ""
)
