package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The primary is an absolute system path, so a test that wanted to exercise the
// success branch would have to be root and would leave a file on the developer's
// machine. What is testable without either is the fallback, which is the branch
// every non-root run of FleetFix takes -- and the one that decides whether a dev
// run produces an audit trail at all.

func TestAuditPathFallsBackToTheStateDirectoryWhenVarLogRefuses(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: /var/log is writable, so there is no fallback to observe")
	}
	state := t.TempDir()
	p := Paths{StateDir: state}

	got, err := p.AuditPath()
	if err == nil {
		t.Fatalf("AuditPath() returned %q with no reason; the primary must have been rejected", got)
	}
	want := filepath.Join(state, AuditFallbackFile)
	if got != want {
		t.Fatalf("AuditPath() = %q, want the state-directory fallback %q", got, want)
	}
	// The directory has to exist before the writer opens the file, because the
	// writer is given a path and not a directory to create.
	if fi, staterr := os.Stat(state); staterr != nil || !fi.IsDir() {
		t.Fatalf("state directory %q is not ready for the writer: %v", state, staterr)
	}
}

// The error is the operator-facing half of the fallback: it has to name the path
// that was refused, or `doctor` prints "falling back" with nothing to act on.
func TestAuditPathReasonNamesTheRefusedPath(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: /var/log is writable, so there is no reason to report")
	}
	_, err := Paths{StateDir: t.TempDir()}.AuditPath()
	if err == nil {
		t.Fatal("no reason returned for the fallback")
	}
	if !strings.Contains(err.Error(), AuditLogPath) {
		t.Fatalf("reason %q does not name %q", err, AuditLogPath)
	}
}

// A state directory that cannot be created is still answered with a path. The
// writer's open reports the real error, and it reports it somewhere a caller can
// show it; failing here would leave the caller with no path and a message about
// a directory it never asked for.
func TestAuditPathStillAnswersWhenTheFallbackDirectoryCannotBeCreated(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: mkdir under a file still fails, but /var/log succeeds first")
	}
	blocker := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(blocker, "fleetfix")

	got, err := Paths{StateDir: state}.AuditPath()
	if err == nil {
		t.Fatal("expected the primary's refusal to be reported")
	}
	if want := filepath.Join(state, AuditFallbackFile); got != want {
		t.Fatalf("AuditPath() = %q, want %q", got, want)
	}
}

func TestReleaseCachePathSitsUnderTheCacheDirectory(t *testing.T) {
	p := Paths{CacheDir: "/home/gn/.cache/fleetfix"}
	if got, want := p.ReleaseCachePath(), "/home/gn/.cache/fleetfix/release_check.json"; got != want {
		t.Fatalf("ReleaseCachePath() = %q, want %q", got, want)
	}
}

// v1 spelled this path as USER_CACHE_DIR / "release_check.json" and a 1.6.0 host
// that upgrades in place keeps its cache file. Renaming it would not break
// anything, but it would silently discard one hour of rate-limit headroom on
// every host in the fleet at the moment they all check at once.
func TestReleaseCacheFileNameMatchesV1(t *testing.T) {
	if ReleaseCacheFile != "release_check.json" {
		t.Fatalf("ReleaseCacheFile = %q; v1 wrote release_check.json", ReleaseCacheFile)
	}
}

func TestTouchCreatesTheParentAndLeavesAnExistingFileAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "deeper", "audit.log")

	if err := touch(path); err != nil {
		t.Fatalf("touch: %v", err)
	}
	if err := os.WriteFile(path, []byte("existing record\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := touch(path); err != nil {
		t.Fatalf("second touch: %v", err)
	}

	// O_APPEND rather than O_TRUNC: a touch that emptied the trail on every
	// launch would destroy exactly the records an operator came looking for.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "existing record\n" {
		t.Fatalf("touch rewrote the file: %q", data)
	}
}

// A Paths with no state directory has nowhere to fall back to. Joining onto an
// empty directory yields the bare name "audit.log", which resolves against
// whatever directory the process happens to be in -- so the same tool run from
// three places would leave three partial trails, each looking like the whole one.
func TestAuditPathRefusesToFallBackToTheWorkingDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: /var/log takes the write and there is no fallback")
	}
	path, reason := Paths{}.AuditPath()
	if reason == nil {
		t.Fatal("no reason was reported for a fallback that could not be prepared")
	}
	if path != AuditLogPath {
		t.Errorf("AuditPath = %q, want the primary %q rather than a relative name", path, AuditLogPath)
	}
	if !filepath.IsAbs(path) {
		t.Errorf("AuditPath = %q, which resolves against the working directory", path)
	}
}
