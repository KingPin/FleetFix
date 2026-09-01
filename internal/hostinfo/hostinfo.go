// Package hostinfo answers "which machine is this?" for the report envelope.
//
// It returns report.Host rather than a struct of its own. The wire shape is the
// contract, and a second struct with the same six fields would need a mapping
// function that is one transposition away from reporting the arch as the kernel
// with nothing to catch it.
//
// Nothing here returns an error. A host whose /etc/os-release is unreadable is
// still a host worth reporting on, and a `check --json` that refused to produce a
// document because it could not name the distro would be a far worse outcome than
// one with an empty distro. Every field is best-effort and empty on failure --
// empty, never a guess: v1 substituted the literal string "Unknown Linux", which
// a consumer grouping by distro would happily treat as a distro.
package hostinfo

import (
	"io/fs"
	"os"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/report"
)

// EtcDir is where os-release lives. Not in hostfs, which is deliberately about
// the two pseudo-filesystems; /etc is an ordinary directory and stretching that
// package to cover it would blur what it is for.
const EtcDir = "/etc"

// Paths read, relative to their roots.
const (
	hostnamePath = "sys/kernel/hostname"
	kernelPath   = "sys/kernel/osrelease"
	bootIDPath   = "sys/kernel/random/boot_id"
	// os-release, relative to /etc. No /usr/lib fallback: on every distro that
	// keeps the canonical copy there, /etc/os-release is a symlink to it, and
	// os.DirFS follows that. A host with neither has genuinely not said what it
	// is, which is what an empty distro means.
	osReleasePath = "os-release"
)

// Detect reads the live host.
func Detect() report.Host {
	return From(hostfs.New().Proc, os.DirFS(EtcDir), Machine())
}

// From reads a staged tree, which is what a test and a future
// container-introspection mode drive.
//
// proc is a /proc root and etc is an /etc root. arch is passed in rather than
// read, because the machine name comes from a syscall and not from a file; that
// keeps the one unfakeable fact at the edge instead of making every test of this
// function depend on the architecture it runs on.
func From(proc, etc fs.FS, arch string) report.Host {
	return report.Host{
		Hostname: trimmed(proc, hostnamePath),
		OS:       "linux",
		Arch:     arch,
		Kernel:   trimmed(proc, kernelPath),
		Distro:   PrettyName(read(etc, osReleasePath)),
		BootID:   trimmed(proc, bootIDPath),
	}
}

// Machine returns the kernel's name for this architecture -- x86_64, aarch64 --
// which is what `uname -m` prints and what an operator's inventory records.
//
// The kernel's answer rather than runtime.GOARCH: those disagree exactly when a
// binary is running under emulation or in a foreign-arch container, which is the
// one case where knowing the difference matters. Empty if the syscall fails,
// which on Linux it does not.
func Machine() string {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return ""
	}
	return nullTerminated(u.Machine[:])
}

// PrettyName extracts PRETTY_NAME from os-release content.
//
// os-release is shell-compatible assignments, so a value may be bare, single- or
// double-quoted. Unquoted values are the same either way in practice, but taking
// the quotes off is what makes "Debian GNU/Linux 12 (bookworm)" read as a distro
// name and not as a quoted string, and every consumer would otherwise strip them
// itself.
//
// Empty when the file has no PRETTY_NAME, which is the honest answer: a host that
// does not say what it is has not said what it is.
func PrettyName(osRelease string) string {
	for line := range strings.Lines(osRelease) {
		key, value, found := strings.Cut(strings.TrimSpace(line), "=")
		if !found || key != "PRETTY_NAME" {
			continue
		}
		return unquote(strings.TrimSpace(value))
	}
	return ""
}

// unquote removes one matched pair of surrounding quotes.
//
// Not a shell unescaper. os-release permits backslash escapes inside a quoted
// value, and no distro in the wild puts one in PRETTY_NAME; implementing half a
// shell parser to handle a case that does not occur would be more code to get
// wrong than the case is worth. A value with an escape in it comes through with
// the backslash, which is visibly odd rather than silently wrong.
func unquote(s string) string {
	for _, q := range []string{`"`, `'`} {
		if len(s) >= 2 && strings.HasPrefix(s, q) && strings.HasSuffix(s, q) {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// nullTerminated reads a C string out of a fixed-size kernel buffer.
func nullTerminated(b []byte) string {
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		return string(b[:i])
	}
	return string(b)
}

func trimmed(fsys fs.FS, name string) string {
	s, err := hostfs.ReadTrimmed(fsys, name)
	if err != nil {
		return ""
	}
	return s
}

func read(fsys fs.FS, name string) string {
	s, err := hostfs.ReadFile(fsys, name)
	if err != nil {
		return ""
	}
	return s
}
