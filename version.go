package main

import (
	_ "embed"
	"runtime/debug"
	"strings"
)

// version may be supplied by a release build with -ldflags '-X main.version=...'.
var version string

//go:embed VERSION
var sourceVersion string

// currentVersion returns release-linker metadata, a published Go module version,
// or the embedded source version, in precedence order. Conventional release v
// prefixes are removed from linker and module versions.
func currentVersion() string {
	if version != "" {
		return strings.TrimPrefix(strings.TrimSpace(version), "v")
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return strings.TrimPrefix(info.Main.Version, "v")
	}
	return strings.TrimSpace(sourceVersion)
}
