// Package assets embeds the versioned deployment recipes and template build
// scripts that ship inside release executables, so installs never depend on
// a source checkout.
package assets

import "embed"

// Files holds deploy/compose and images/ubuntu-24.04 from the exact release
// the executable was built from.
//
//go:embed deploy/compose images/ubuntu-24.04
var Files embed.FS
