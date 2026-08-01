package version

import (
	"runtime/debug"
	"strings"
	"testing"
)

func info(v string) *debug.BuildInfo {
	return &debug.BuildInfo{Main: debug.Module{Version: v}}
}

func TestResolve(t *testing.T) {
	tests := []struct {
		name  string
		stamp string
		info  *debug.BuildInfo
		want  string
	}{
		{"link-time stamp wins", "2.0.0", info("v1.9.9"), "2.0.0"},
		{"leading v is stripped so --version matches the tag minus v", "v2.0.0", nil, "2.0.0"},
		{"stray whitespace from a shell-built ldflag", "  2.0.1\n", nil, "2.0.1"},
		{"go install records the module version", "", info("v2.0.0"), "2.0.0"},
		{"a working-tree build identifies nothing", "", info("(devel)"), "dev"},
		{"no stamp and no build info", "", nil, "dev"},
		{"empty build-info version is not a version", "", info(""), "dev"},
		{"a stamp of only whitespace does not shadow the fallback", " ", info("v1.6.0"), "1.6.0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolve(tt.stamp, tt.info); got != tt.want {
				t.Errorf("resolve(%q, %v) = %q, want %q", tt.stamp, tt.info, got, tt.want)
			}
		})
	}
}

func TestVersionIsNeverEmpty(t *testing.T) {
	// Whatever the build, something has to answer `fleetfix --version`.
	if Version() == "" {
		t.Fatal("Version() is empty")
	}
}

func TestUserAgentCarriesVersionAndRepo(t *testing.T) {
	ua := UserAgent()
	if !strings.HasPrefix(ua, "fleetfix/") {
		t.Errorf("UserAgent() = %q, want a fleetfix/ prefix", ua)
	}
	if !strings.Contains(ua, Version()) {
		t.Errorf("UserAgent() = %q, want it to contain version %q", ua, Version())
	}
	if !strings.Contains(ua, Repo) {
		t.Errorf("UserAgent() = %q, want it to contain %q", ua, Repo)
	}
}

func TestStampedBuildReportsTheStamp(t *testing.T) {
	// Covers the stamped path when the suite is run with the same -ldflags as a
	// release build: that the stamp is preferred over build info and arrives
	// cleaned.
	//
	// It deliberately does not guard a mistyped -X symbol path. -X silently does
	// nothing when the path is wrong, so `stamped` would be empty and this would
	// skip, not fail. The guard for that is the release workflow asserting the
	// built binary's `--version` equals the tag minus its "v" -- an assertion on
	// the real artifact, which no test binary can stand in for.
	if stamped == "" {
		t.Skip("no -ldflags stamp in this build")
	}
	if got, want := Version(), clean(stamped); got != want {
		t.Errorf("Version() = %q, want the stamped %q", got, want)
	}
}

func TestVersionHasNoLeadingV(t *testing.T) {
	// The release workflow compares `--version` against the tag with its "v"
	// removed, so a "v" leaking through here fails the release, not a test.
	if strings.HasPrefix(Version(), "v") {
		t.Errorf("Version() = %q, want no leading v", Version())
	}
}
