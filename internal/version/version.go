// Package version reports the build's identity.
//
// The string is stamped at link time:
//
//	go build -ldflags "-X github.com/KingPin/FleetFix/v2/internal/version.stamped=2.0.0"
//
// which is the point of the package. v1.x carried the version in two places --
// src/fleetfix/__init__.py and pyproject.toml -- so a release meant editing both
// and a missed one shipped a binary that lied about itself. Here the git tag is
// the only source and the release workflow passes it through.
//
// Values are bare, with no leading "v": v1.6.0's `--version` printed "1.6.0",
// the release asserts `--version` equals the tag minus its "v", and the updater
// compares versions by PEP 440, which tolerates either form.
package version

import (
	"runtime/debug"
	"strings"
)

// Repo is the canonical project URL, used in the User-Agent so an operator
// reading a server log can find out what is calling them.
const Repo = "https://github.com/KingPin/FleetFix"

// stamped is overwritten via -ldflags at build time. Empty in a plain `go build`.
var stamped string

// Version returns the running binary's version, or "dev" when nothing identifies it.
func Version() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		info = nil
	}
	return resolve(stamped, info)
}

// UserAgent identifies this binary to the GitHub API and any probed HTTP endpoint.
func UserAgent() string {
	return "fleetfix/" + Version() + " (+" + Repo + ")"
}

// resolve picks the best available version, preferring the link-time stamp.
//
// Split out from Version so both branches are testable: the -ldflags path cannot
// be exercised from inside the test binary that would be asserting on it.
//
// The fallback matters for `go install`, which records the module version in the
// build info but passes no ldflags -- without it every such build would claim to
// be "dev" and an operator's bug report would not say what they were running.
// A build from a working tree reports "(devel)", which identifies nothing.
func resolve(stamp string, info *debug.BuildInfo) string {
	if v := clean(stamp); v != "" {
		return v
	}
	if info != nil && info.Main.Version != "(devel)" {
		if v := clean(info.Main.Version); v != "" {
			return v
		}
	}
	return "dev"
}

func clean(v string) string {
	return strings.TrimPrefix(strings.TrimSpace(v), "v")
}
