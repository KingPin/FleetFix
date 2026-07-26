// Package hostfs is the read seam for the two pseudo-filesystems FleetFix treats
// as data sources: /proc and /sys.
//
// It exists for the same reason internal/cmdrun does. Roughly a third of v1's
// collectors take an injectable Path default (`source: Path = Path("/proc/uptime")`)
// and are therefore testable; the rest hardcode the path and are not. Making the
// root an fs.FS rather than a directory string means a test needs no temp
// directory at all -- fstest.MapFS is the whole fixture -- and CI does not have to
// own a /proc that looks like a Raspberry Pi's.
//
// Two roots rather than one at "/" because fs.FS paths are relative and unrooted
// by contract. A caller reads "uptime" from Proc and "class/net/eth0/operstate"
// from Sys; a leading slash is a programming error and is reported as one.
//
// What belongs here is reading and classifying failures. What does not is any
// knowledge of what the bytes mean -- parsing /proc/meminfo or enumerating PIDs is
// internal/core's job, and putting it here would recreate the layering v1's
// modules/ boundary was drawn to prevent.
package hostfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Default roots. Overridable through At, which is how a test drives a captured
// tree and how a future container-introspection mode would read a different one.
const (
	DefaultProc = "/proc"
	DefaultSys  = "/sys"
)

// Host bundles the roots a collector reads from.
//
// A plain struct of two fs.FS rather than an interface: there is exactly one
// behaviour to vary (where the bytes come from), fs.FS already varies it, and an
// interface on top would only add a second name for the same thing.
type Host struct {
	Proc fs.FS
	Sys  fs.FS
}

// New returns a Host reading the live /proc and /sys.
func New() Host {
	return At(DefaultProc, DefaultSys)
}

// At returns a Host rooted at the given directories.
func At(procDir, sysDir string) Host {
	return Host{Proc: os.DirFS(procDir), Sys: os.DirFS(sysDir)}
}

// ReadFile reads a whole file as a string.
//
// It does not use the file's reported size, because every file under /proc reports
// st_size 0 while returning content -- the trap that makes a hand-rolled
// read-into-a-sized-buffer return nothing at all here. fs.ReadFile treats size as
// a hint only, which is the behaviour that is wanted.
func ReadFile(fsys fs.FS, name string) (string, error) {
	if !fs.ValidPath(name) {
		return "", fmt.Errorf("hostfs: %q is not a valid path: names are relative to the root, with no leading slash", name)
	}
	b, err := fs.ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ReadTrimmed reads a file and strips surrounding whitespace.
//
// The common shape in sysfs: one value and a trailing newline, as in
// class/net/eth0/operstate or class/thermal/thermal_zone0/type.
//
// Python's whitespace, not Go's: v1 spells this read_text().strip(), and str.strip()
// also removes U+001C..U+001F, which strings.TrimSpace leaves in place. The value
// reaches an operator's screen verbatim -- a link whose operstate came back as
// "\x1cup" would be graded against the string "down" and pass while reading as
// garbage -- so the two implementations must agree on the bytes, not merely on the
// verdict.
func ReadTrimmed(fsys fs.FS, name string) (string, error) {
	s, err := ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	return strings.TrimFunc(s, pytext.IsSpace), nil
}

// ReadInt reads a file holding a single integer, such as a thermal zone's
// millidegree temp or a block device's size in 512-byte sectors.
func ReadInt(fsys fs.FS, name string) (int64, error) {
	s, err := ReadTrimmed(fsys, name)
	if err != nil {
		return 0, err
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("hostfs: %s: %w", name, err)
	}
	return n, nil
}

// ReadFileLossy reads a file, substituting U+FFFD for content that is not valid
// UTF-8, the way Python's bytes.decode("utf-8", errors="replace") does.
//
// For the one place it is needed: a process's comm in /proc/<pid>/stat is arbitrary
// bytes, and v1 reads it that way. Go would otherwise carry the invalid bytes
// through into a string and out into JSON, where encoding/json substitutes U+FFFD
// anyway -- but only at the boundary, so any comparison, length check, or column
// truncation in between sees different data than Python saw, and the M2 harness
// reports it as a divergence with no obvious cause.
//
// One U+FFFD per *maximal ill-formed subsequence*, which is Unicode TR#36's
// recommended practice and, verified against CPython, what Python does. Neither
// obvious shortcut matches it:
//
//   - utf8.DecodeRuneInString reports (RuneError, 1) for every bad byte, so a
//     truncated three-byte sequence such as "\xe4\xb8" would emit two replacements
//     where Python emits one.
//   - strings.ToValidUTF8 collapses a whole run of bad bytes into one replacement,
//     so "\xff\xfe" -- two independently invalid start bytes -- would emit one
//     where Python emits two.
func ReadFileLossy(fsys fs.FS, name string) (string, error) {
	s, err := ReadFile(fsys, name)
	if err != nil {
		return "", err
	}
	return replaceInvalidUTF8(s), nil
}

func replaceInvalidUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if r, size := utf8.DecodeRuneInString(s[i:]); r != utf8.RuneError || size > 1 {
			// Well-formed, including a literal U+FFFD already in the input, which
			// is copied through rather than re-replaced.
			b.WriteString(s[i : i+size])
			i += size
			continue
		}
		b.WriteRune(utf8.RuneError)
		i += maximalSubpartLen(s[i:])
	}
	return b.String()
}

// maximalSubpartLen returns how many bytes at the start of s form one ill-formed
// subsequence: the leading byte plus every following byte that could still have
// been part of the sequence it announced. Always at least 1, so the caller advances.
//
// The per-lead-byte ranges are not decoration. A lead byte constrains its first
// continuation more tightly than 80..BF in three cases -- E0 excludes overlong
// encodings, ED excludes the surrogate range, F0 and F4 bound the plane -- and a
// byte outside the tighter range starts a new error rather than extending this one.
// Treating those as continuations would swallow a byte Python reports separately.
func maximalSubpartLen(s string) int {
	b0 := s[0]
	var want int
	lo, hi := byte(0x80), byte(0xBF)
	switch {
	case b0 >= 0xC2 && b0 <= 0xDF:
		want = 2
	case b0 == 0xE0:
		want, lo = 3, 0xA0
	case b0 >= 0xE1 && b0 <= 0xEC, b0 >= 0xEE && b0 <= 0xEF:
		want = 3
	case b0 == 0xED:
		want, hi = 3, 0x9F
	case b0 == 0xF0:
		want, lo = 4, 0x90
	case b0 >= 0xF1 && b0 <= 0xF3:
		want = 4
	case b0 == 0xF4:
		want, hi = 4, 0x8F
	default:
		// A continuation byte with nothing to continue (80..BF), or a lead byte no
		// valid encoding uses (C0, C1, F5..FF). One error, one byte.
		return 1
	}

	n := 1
	for ; n < want && n < len(s); n++ {
		c := s[n]
		if c < lo || c > hi {
			break
		}
		// Only the first continuation carries the tightened range.
		lo, hi = 0x80, 0xBF
	}
	return n
}

// Exists reports whether a path is present.
//
// Convenience for the "is this kernel feature compiled in" question that reads
// better as a condition than as a discarded error, mirroring v1's
// `if not root.exists(): return []`.
//
// Presence, not readability: stat needs no read permission on the file itself, so
// this returns true for a file whose content cannot be read. It is therefore not a
// precondition for reading -- when the content is wanted, read it and classify the
// error with IsAbsent. Checking first is also a race on a filesystem where entries
// appear and vanish with processes.
func Exists(fsys fs.FS, name string) bool {
	if !fs.ValidPath(name) {
		return false
	}
	_, err := fs.Stat(fsys, name)
	return err == nil
}

// IsAbsent reports whether err means the path is simply not there.
//
// The single most common non-failure in this package, and it must not be reported
// as a fault: /sys/class/thermal does not exist on most VMs, /sys/block is empty in
// a container, and a /proc/<pid> entry vanishes the moment the process exits --
// which happens routinely mid-walk, not exceptionally. Each of those means "no
// data", and a collector's answer is "unavailable", not "error".
//
// ESRCH is included because a read of a /proc/<pid> file whose process is exiting
// can surface as "no such process" rather than "no such file".
func IsAbsent(err error) bool {
	return errors.Is(err, fs.ErrNotExist) ||
		errors.Is(err, syscall.ENOTDIR) ||
		errors.Is(err, syscall.ESRCH)
}

// IsDenied reports whether err means the path exists but is not readable.
//
// Distinct from IsAbsent on purpose: another user's /proc/<pid>/stat is denied, not
// missing, and a process list that silently dropped those rows would under-report
// exactly the processes an operator is most likely hunting. The right answer is a
// partial result that says so, which the caller can only produce if it can tell the
// two apart.
func IsDenied(err error) bool {
	return errors.Is(err, fs.ErrPermission)
}
