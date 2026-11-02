// Package buildversion formats the module version embedded in a Go binary.
package buildversion

import (
	"runtime/debug"
	"strings"
)

// Version returns the embedded module version, or devel when unavailable.
func Version() string {
	version := "devel"
	if info, ok := debug.ReadBuildInfo(); ok && strings.HasPrefix(info.Main.Version, "v") {
		version, _, _ = strings.Cut(info.Main.Version, "-")
		version = strings.TrimSuffix(version, "+dirty")
	}
	return version
}

// String returns the program name and its release version.
func String(program string) string {
	return program + " (" + Version() + ")"
}
