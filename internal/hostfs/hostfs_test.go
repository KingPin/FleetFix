package hostfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/fstest"
	"unicode/utf8"
)

// A stand-in for the trees the collectors will read, laid out exactly as the real
// pseudo-filesystems are so a path in a test is a path on a host.
func fakeHost() Host {
	return Host{
		Proc: fstest.MapFS{
			"uptime":  {Data: []byte("123456.78 987654.32\n")},
			"loadavg": {Data: []byte("0.12 0.34 0.56 1/234 5678\n")},
			"net/dev": {Data: []byte("Inter-|   Receive\n")},
		},
		Sys: fstest.MapFS{
			// MapFS has no symlinks, so entries sit at the paths a resolved
			// /sys/class/net/<iface> read reaches.
			"class/net/eth0/operstate":         {Data: []byte("up\n")},
			"class/thermal/thermal_zone0/temp": {Data: []byte("48000\n")},
			"class/thermal/thermal_zone0/type": {Data: []byte("x86_pkg_temp\n")},
		},
	}
}

func TestReadFileReturnsTheWholeContent(t *testing.T) {
	h := fakeHost()
	got, err := ReadFile(h.Proc, "uptime")
	if err != nil {
		t.Fatalf("ReadFile errored: %v", err)
	}
	// Untrimmed: the caller decides, and a parser fed /proc/net/dev needs its
	// newlines.
	if got != "123456.78 987654.32\n" {
		t.Errorf("ReadFile = %q, want the bytes verbatim", got)
	}
}

func TestReadTrimmed(t *testing.T) {
	h := fakeHost()
	got, err := ReadTrimmed(h.Sys, "class/net/eth0/operstate")
	if err != nil {
		t.Fatalf("ReadTrimmed errored: %v", err)
	}
	if got != "up" {
		t.Errorf("ReadTrimmed = %q, want %q", got, "up")
	}
}

// ReadTrimmed must strip what Python's str.strip() strips, not what
// strings.TrimSpace does. Every expected value here was measured against CPython;
// U+001C..U+001F are the whole delta, and they are the reason this is not TrimSpace.
func TestReadTrimmedStripsPythonsWhitespace(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"trailing newline", "up\n", "up"},
		{"surrounding spaces", "  up  \n", "up"},
		{"file separators", "\x1cup\x1f", "up"},
		// Spelled as code points, not bytes: these are two-byte UTF-8 sequences in
		// the file, and a lone 0x85 byte would be invalid UTF-8 rather than U+0085.
		{"next line and nbsp", "\u0085up\u00a0", "up"},
		{"vertical tab and form feed", "\x0bup\x0c", "up"},
		{"nothing but separators", "\x1c\x1d\x1e\x1f", ""},
		{"empty", "", ""},
		{"only newlines", "\n\n", ""},
		// NUL is not whitespace to Python, and an operstate that came back with one
		// is a value worth seeing rather than one to quietly clean up.
		{"nul is not whitespace", "up\x00", "up\x00"},
		{"interior space is left alone", "a b\n", "a b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{"operstate": {Data: []byte(tt.data)}}
			got, err := ReadTrimmed(fsys, "operstate")
			if err != nil {
				t.Fatalf("ReadTrimmed errored: %v", err)
			}
			if got != tt.want {
				t.Errorf("ReadTrimmed(%q) = %q, want %q", tt.data, got, tt.want)
			}
		})
	}
}

func TestReadInt(t *testing.T) {
	h := fakeHost()
	got, err := ReadInt(h.Sys, "class/thermal/thermal_zone0/temp")
	if err != nil {
		t.Fatalf("ReadInt errored: %v", err)
	}
	if got != 48000 {
		t.Errorf("ReadInt = %d, want 48000", got)
	}
}

func TestReadIntRejectsNonNumericContent(t *testing.T) {
	fsys := fstest.MapFS{
		"garbage": {Data: []byte("not a number\n")},
		"empty":   {Data: []byte("")},
		// A sysfs file can report a kernel-side error as text rather than failing
		// the read. Grading that as a temperature would be worse than reporting no
		// reading at all.
		"partial": {Data: []byte("48000 extra\n")},
	}
	for _, name := range []string{"garbage", "empty", "partial"} {
		t.Run(name, func(t *testing.T) {
			if _, err := ReadInt(fsys, name); err == nil {
				t.Errorf("ReadInt(%q) returned no error", name)
			} else if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name the file", err)
			}
		})
	}
}

func TestAnAbsolutePathIsRejectedWithAUsefulMessage(t *testing.T) {
	// The mistake this catches is real and easy: a caller pastes the host path it
	// is reading and gets "invalid argument" from deep in io/fs, with no hint that
	// names are root-relative.
	h := fakeHost()
	_, err := ReadFile(h.Proc, "/proc/uptime")
	if err == nil {
		t.Fatal("ReadFile accepted an absolute path")
	}
	if !strings.Contains(err.Error(), "leading slash") {
		t.Errorf("error %q does not explain the path convention", err)
	}
	if IsAbsent(err) {
		t.Error("IsAbsent matched a malformed path; a caller would report unavailable instead of fixing the bug")
	}
}

func TestReadersAgreeOnRejectingAnInvalidPath(t *testing.T) {
	h := fakeHost()
	if _, err := ReadTrimmed(h.Proc, "/uptime"); err == nil {
		t.Error("ReadTrimmed accepted an absolute path")
	}
	if _, err := ReadInt(h.Proc, "/uptime"); err == nil {
		t.Error("ReadInt accepted an absolute path")
	}
	if _, err := ReadFileLossy(h.Proc, "/uptime"); err == nil {
		t.Error("ReadFileLossy accepted an absolute path")
	}
	if Exists(h.Proc, "/uptime") {
		t.Error("Exists reported true for an invalid path")
	}
}

func TestExists(t *testing.T) {
	h := fakeHost()
	if !Exists(h.Sys, "class/thermal/thermal_zone0/temp") {
		t.Error("Exists = false for a present file")
	}
	if !Exists(h.Sys, "class/thermal") {
		t.Error("Exists = false for a present directory")
	}
	// The case that matters: no thermal sensors at all, which is most VMs.
	if Exists(h.Sys, "class/thermal/thermal_zone9/temp") {
		t.Error("Exists = true for an absent file")
	}
}

func TestIsAbsentCoversTheWaysAPathCanBeMissing(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "not exist", err: fs.ErrNotExist, want: true},
		// A /proc/<pid> entry vanishes the moment the process exits, which happens
		// routinely mid-walk. Reading a file under it can report either of these.
		{name: "wrapped ENOENT", err: &fs.PathError{Op: "open", Path: "1234/stat", Err: syscall.ENOENT}, want: true},
		{name: "ESRCH", err: &fs.PathError{Op: "read", Path: "1234/stat", Err: syscall.ESRCH}, want: true},
		// A path component that turned out to be a file, not a directory.
		{name: "ENOTDIR", err: &fs.PathError{Op: "open", Path: "uptime/nope", Err: syscall.ENOTDIR}, want: true},
		// Present but unreadable is a different answer, and conflating the two is
		// how a process list silently drops other users' rows.
		{name: "permission", err: fs.ErrPermission, want: false},
		{name: "EIO", err: &fs.PathError{Op: "read", Path: "x", Err: syscall.EIO}, want: false},
		{name: "unrelated", err: errors.New("boom"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAbsent(tt.err); got != tt.want {
				t.Errorf("IsAbsent(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestIsDenied(t *testing.T) {
	if !IsDenied(&fs.PathError{Op: "open", Path: "1/stat", Err: syscall.EACCES}) {
		t.Error("IsDenied = false for EACCES")
	}
	if !IsDenied(&fs.PathError{Op: "open", Path: "1/stat", Err: syscall.EPERM}) {
		t.Error("IsDenied = false for EPERM")
	}
	if IsDenied(fs.ErrNotExist) {
		t.Error("IsDenied = true for a missing path")
	}
	if IsDenied(nil) {
		t.Error("IsDenied = true for nil")
	}
}

func TestAMissingFileClassifiesAsAbsentNotAsAFailure(t *testing.T) {
	h := fakeHost()
	_, err := ReadFile(h.Sys, "class/thermal/thermal_zone0/crit_temp")
	if err == nil {
		t.Fatal("ReadFile returned no error for a missing file")
	}
	if !IsAbsent(err) {
		t.Errorf("IsAbsent(%v) = false; a host without this sensor is supported, not broken", err)
	}
	if IsDenied(err) {
		t.Errorf("IsDenied(%v) = true for a missing file", err)
	}
}

func TestReadFileLossyMatchesPythonsReplaceErrorHandler(t *testing.T) {
	// v1 reads /proc/<pid>/stat with errors="replace", and a process can put
	// arbitrary bytes in its comm. Every want below was produced by CPython's
	// bytes.decode("utf-8", errors="replace") rather than reasoned about, because
	// the rule is not the obvious one: replacement is per maximal ill-formed
	// subsequence, so a truncated multi-byte sequence yields ONE U+FFFD while two
	// independently invalid bytes yield two. Getting this wrong shows up in M2 as a
	// divergence with no visible cause.
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "valid ascii", in: "sshd", want: "sshd"},
		{name: "valid multibyte", in: "café", want: "café"},
		{name: "lone continuation byte", in: "a\x80b", want: "a�b"},
		// One replacement: \xe4\xb8 is a truncated three-byte sequence, so both
		// bytes are one error. Per-byte replacement would give two.
		{name: "truncated 3-byte sequence", in: "a\xe4\xb8b", want: "a�b"},
		// Two replacements: neither byte can start a sequence, so they are separate
		// errors. Run-collapsing would give one.
		{name: "two invalid start bytes", in: "\xff\xfe", want: "��"},
		{name: "invalid at end", in: "cmd\xc3", want: "cmd�"},
		{name: "truncated 4-byte at end", in: "x\xf0\x9f\x92", want: "x�"},
		// E0 A0..BF only: 0x80 is an overlong-encoding attempt, so it does not
		// extend the sequence and is its own error.
		{name: "overlong lead E0 then 80", in: "\xe0\x80\x80", want: "���"},
		// ED 80..9F only: 0xA0 would encode a surrogate.
		{name: "surrogate lead ED then A0", in: "\xed\xa0\x80", want: "���"},
		// F4 80..8F only: 0x90 is past the last plane.
		{name: "out of range lead F4 then 90", in: "\xf4\x90\x80\x80", want: "����"},
		{name: "C0 is never a lead byte", in: "\xc0\xaf", want: "��"},
		// The lead-byte ranges with no tightened continuation rule: one error each,
		// however many valid continuations the truncated sequence managed.
		{name: "truncated F1 lead", in: "\xf1\x80", want: "�"},
		{name: "truncated F2 lead then ascii", in: "\xf2\x80\x80x", want: "�x"},
		{name: "truncated EE lead", in: "\xee\x80", want: "�"},
		{name: "bare C2 lead", in: "\xc2", want: "�"},
		// An input that already contains U+FFFD is passed through, not re-replaced.
		{name: "literal replacement char", in: "a�b", want: "a�b"},
		{name: "empty", in: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fsys := fstest.MapFS{"stat": {Data: []byte(tt.in)}}
			got, err := ReadFileLossy(fsys, "stat")
			if err != nil {
				t.Fatalf("ReadFileLossy errored: %v", err)
			}
			if got != tt.want {
				t.Errorf("ReadFileLossy(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// FuzzReplaceInvalidUTF8 covers the invariants that hold without an oracle, so
// they keep holding after the Python is deleted at M7. Table cases above pin the
// agreement with CPython; this pins that the output is always safe to hand onward.
func FuzzReplaceInvalidUTF8(f *testing.F) {
	for _, seed := range []string{
		"sshd", "café", "a\x80b", "a\xe4\xb8b", "\xff\xfe", "\xe0\x80\x80",
		"\xed\xa0\x80", "\xf4\x90\x80\x80", "(python3.11)", "",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := replaceInvalidUTF8(in)
		if !utf8.ValidString(got) {
			t.Fatalf("replaceInvalidUTF8(%q) = %q, which is still not valid UTF-8", in, got)
		}
		// Idempotent: a second pass must not replace the replacements it made, or a
		// value that crosses this boundary twice degrades each time.
		if again := replaceInvalidUTF8(got); again != got {
			t.Fatalf("not idempotent: %q -> %q -> %q", in, got, again)
		}
		// Valid input is returned byte-for-byte. A reader that quietly rewrote
		// well-formed content would corrupt every parser downstream of it.
		if utf8.ValidString(in) && got != in {
			t.Fatalf("valid input %q was rewritten to %q", in, got)
		}
	})
}

func TestReadFileKeepsInvalidBytesUntouched(t *testing.T) {
	// The plain reader must not quietly sanitise. A parser working on /proc/net/dev
	// wants the bytes it was given, and choosing the lossy variant is the caller's
	// decision to make explicitly.
	fsys := fstest.MapFS{"stat": {Data: []byte("a\x80b")}}
	got, err := ReadFile(fsys, "stat")
	if err != nil {
		t.Fatalf("ReadFile errored: %v", err)
	}
	if got != "a\x80b" {
		t.Errorf("ReadFile = %q, want the raw bytes", got)
	}
}

func TestAtRootsBothFilesystems(t *testing.T) {
	dir := t.TempDir()
	procDir := filepath.Join(dir, "proc")
	sysDir := filepath.Join(dir, "sys")
	for path, data := range map[string]string{
		filepath.Join(procDir, "uptime"):    "42.0 1.0\n",
		filepath.Join(sysDir, "kernel_mmu"): "on\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir failed: %v", err)
		}
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatalf("write failed: %v", err)
		}
	}

	h := At(procDir, sysDir)
	if got, err := ReadTrimmed(h.Proc, "uptime"); err != nil || got != "42.0 1.0" {
		t.Errorf("Proc read = %q, %v", got, err)
	}
	if got, err := ReadTrimmed(h.Sys, "kernel_mmu"); err != nil || got != "on" {
		t.Errorf("Sys read = %q, %v", got, err)
	}
	// The roots must not be interchangeable, or a wrong-root bug reads plausible
	// data from the wrong place.
	if _, err := ReadFile(h.Proc, "kernel_mmu"); !IsAbsent(err) {
		t.Errorf("Proc resolved a Sys path: %v", err)
	}
}

func TestAnUnreadableFileIsDeniedNotAbsent(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads anything; the distinction is unobservable here")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte("x"), 0o000); err != nil {
		t.Fatalf("write failed: %v", err)
	}

	_, err := ReadFile(os.DirFS(dir), "secret")
	if err == nil {
		t.Fatal("ReadFile read a mode-000 file")
	}
	if !IsDenied(err) {
		t.Errorf("IsDenied(%v) = false, want true", err)
	}
	if IsAbsent(err) {
		t.Errorf("IsAbsent(%v) = true; the file exists", err)
	}
	// Exists still says true: stat needs no read permission on the file itself.
	// That is the reason Exists is not a precondition for reading -- a caller that
	// gates on it here proceeds to a read that fails anyway, and one that reports
	// "present" as "fine" hides a permission problem behind a green tick.
	if !Exists(os.DirFS(dir), "secret") {
		t.Error("Exists = false for a present but unreadable file; it reports presence, not readability")
	}
}

// TestAgainstTheRealProcAndSys is the test that would catch the failure no MapFS
// can reproduce: every file under /proc reports st_size 0, so a reader that trusts
// the stat size returns empty content from a file that is not empty. It also proves
// os.DirFS traverses /sys's symlinks -- class/net/<iface> and
// class/thermal/thermal_zone<n> are links into ../../devices, and if DirFS refused
// to follow them the whole Sys root would be useless.
func TestAgainstTheRealProcAndSys(t *testing.T) {
	if _, err := os.Stat(DefaultProc); err != nil {
		t.Skipf("no %s on this machine", DefaultProc)
	}
	h := New()

	uptime, err := ReadTrimmed(h.Proc, "uptime")
	if err != nil {
		t.Fatalf("reading /proc/uptime failed: %v", err)
	}
	if uptime == "" {
		t.Fatal("/proc/uptime read as empty: the reader is trusting the reported size, which is 0 for every procfs file")
	}
	if len(strings.Fields(uptime)) != 2 {
		t.Errorf("/proc/uptime = %q, want two fields", uptime)
	}

	// A path under a /proc/<pid> directory, which is where the disappearing-entry
	// races live.
	if _, err := ReadFileLossy(h.Proc, "self/stat"); err != nil {
		t.Errorf("reading /proc/self/stat failed: %v", err)
	}

	// Symlink traversal. Loopback is present on any Linux host including the
	// container CI runs in.
	if !Exists(h.Sys, "class/net/lo") {
		t.Skip("no /sys/class/net/lo; not a normal Linux host")
	}
	state, err := ReadTrimmed(h.Sys, "class/net/lo/operstate")
	if err != nil {
		t.Fatalf("reading through the /sys/class/net symlink failed: %v", err)
	}
	if state == "" {
		t.Error("operstate read as empty")
	}
}
