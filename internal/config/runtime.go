package config

import (
	"os"
	"path/filepath"
)

// AuditLogPath is where the audit trail lands on a production host.
//
// /var/log rather than the state directory, and not configurable, because the
// trail's value comes from being in the place a fleet's log shipper already
// watches. An operator who could move it could also move it somewhere nothing
// collects, which is indistinguishable from having no audit trail at all.
const AuditLogPath = "/var/log/fleetfix-audit.log"

// AuditFallbackFile is the audit log's name under the state directory, used when
// /var/log will not take a write. The name is different from the primary's on
// purpose: a support bundle that collected both would otherwise hold two files
// called fleetfix-audit.log and no way to say which host role wrote which.
const AuditFallbackFile = "audit.log"

// ReleaseCacheFile holds the last releases-API response under the cache
// directory, so a relaunch loop does not hammer GitHub.
const ReleaseCacheFile = "release_check.json"

// GitHubReleasesURL is where the updater asks what the latest release is.
//
// Not configurable, and here rather than in internal/updater so it sits beside the
// cache file it feeds. An operator who could repoint it could point a fleet's
// self-update at a repository they control, which is the whole supply chain for
// every host running this binary. A fork changes the constant and rebuilds.
const GitHubReleasesURL = "https://api.github.com/repos/KingPin/FleetFix/releases/latest"

// AuditPath reports where this process should write its audit trail, and why it
// is not writing to /var/log when it is not.
//
// The returned error is not a failure. It is the reason for the fallback, and a
// caller that ignores it still gets a usable path -- which is v1's whole
// contract, since v1 returned a bare Path and swallowed the OSError. `doctor`
// wants the reason: "the trail you are looking for is under $XDG_STATE_HOME
// because /var/log/fleetfix-audit.log is not writable by this user" is the
// answer to a question an operator asks after finding an empty file.
//
// The primary is created here rather than left to the writer, because creating
// it is the test: /var/log exists and is root-owned on every target distro, so
// statting it says nothing, and a permission check that raced the open would
// answer for a moment that has passed. Touching the file is the same syscall the
// writer will make.
//
// A failure to prepare the fallback is not reported. The path is returned
// regardless and the writer's open will fail with the real error, at the point
// where a caller has somewhere to put it -- returning it here would mean two
// error paths for one condition, and the second one has more to say.
func (p Paths) AuditPath() (string, error) {
	err := touch(AuditLogPath)
	if err == nil {
		return AuditLogPath, nil
	}
	if p.StateDir == "" {
		// Nowhere to fall back to. Joining onto an empty directory yields the bare
		// name "audit.log", which resolves against whatever directory the process
		// happens to be in -- so a fleet tool run from three places would leave
		// three partial trails and each would look like the whole one. Answering
		// with the primary instead means the caller's open fails with the real
		// permission error, which is a refusal rather than a scattered trail.
		return AuditLogPath, err
	}
	// 0o700, where the primary's parent is 0o755. /var/log/fleetfix-audit.log is
	// meant to be collected by a log shipper running as `adm`; a trail under a
	// user's own state directory has no such reader, and the XDG specification
	// asks for these directories to be user-private.
	//
	// Best-effort: see the note above on why this error is not the one to report.
	_ = os.MkdirAll(p.StateDir, 0o700)
	return filepath.Join(p.StateDir, AuditFallbackFile), err
}

// ReleaseCachePath is where the release checker caches the API response.
func (p Paths) ReleaseCachePath() string {
	return filepath.Join(p.CacheDir, ReleaseCacheFile)
}

// touch creates path and its parent, leaving an existing file untouched.
//
// 0o644 and 0o755 are written out rather than left to a umask, unlike v1's
// Path.touch(0o666) and mkdir(0o777). The audit log is group-readable so that
// `adm` can collect it without root, and a host whose umask was 027 would have
// silently produced a file only root could read -- which is the difference
// between a fleet with a shipped audit trail and one without.
func touch(path string) error {
	// 0o755 on the parent: this is /var/log, which exists at that mode on every
	// target distro, so the argument is only reached on a host where it does not
	// -- and a private /var/log would break every other logger on the box.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil { //nolint:gosec // G301: /var/log, not a private state dir
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644) //nolint:gosec // G304: the path is the argument; naming a file is the whole call
	if err != nil {
		return err
	}
	return f.Close()
}
