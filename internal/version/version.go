// Package version is the running desprawl's version.
package version

import (
	"runtime/debug"
	"strings"

	"golang.org/x/mod/semver"
)

// release is set at release time (.goreleaser.yaml:
// -ldflags "-X github.com/askrike/desprawl/internal/version.release=…").
var release = ""

// Release is the release this binary is ("v0.2.0"), or "" for a build from
// source.
func Release() string {
	if release != "" && !strings.HasPrefix(release, "v") {
		return "v" + release
	}
	return release
}

// String is the version to show: the release, else the module version a
// source build reports (v0.1.11+dirty from a checkout), else "dev".
func String() string {
	if r := Release(); r != "" {
		return r
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}

// Newer reports whether a is a later version than b. Versions that aren't
// semver are never newer.
func Newer(a, b string) bool {
	a, b = canonical(a), canonical(b)
	return semver.IsValid(a) && semver.IsValid(b) && semver.Compare(a, b) > 0
}

func canonical(v string) string {
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	return v
}
