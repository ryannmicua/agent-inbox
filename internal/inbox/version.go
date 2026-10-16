package inbox

import "runtime/debug"

// BuildVersion reports the module version the Go toolchain stamped into the
// running binary: a module version or pseudo-version for `go install` and for
// builds in a git checkout, or "(devel)" when no version is known.
func BuildVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
