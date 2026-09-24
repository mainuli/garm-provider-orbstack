// Package assets embeds the versioned template build scripts that ship
// inside release executables, so template building never depends on a
// source checkout.
package assets

import "embed"

// Files holds images/ubuntu-24.04 from the exact release the executable
// was built from.
//
//go:embed images/ubuntu-24.04
var Files embed.FS
