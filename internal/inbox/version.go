package inbox

import "runtime/debug"

// version is the release version stamped at link time by scripts/release.sh
// with -ldflags "-X github.com/ryannmicua/agent-inbox/internal/inbox.version=vX.Y.Z".
// It is empty in every other build.
var version string

// BuildVersion reports the release version stamped by the release script, or
// else the module version the Go toolchain stamped into the running binary: a
// module version or pseudo-version for `go install` and for builds in a git
// checkout, or "(devel)" when no version is known.
func BuildVersion() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "(devel)"
}
