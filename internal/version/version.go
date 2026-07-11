package version

import "runtime/debug"

// Version returns the build version from module info, or "dev" for un-tagged builds.
func Version() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}
