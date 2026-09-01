package hostinfo

import (
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/report"
)

// arch is whatever Machine() would have returned, held still so the tests that
// are not about the machine name do not depend on the one they run on.
const arch = "x86_64"

// A Debian host as /proc and /etc actually present it: hostname and osrelease
// carry a trailing newline, boot_id is a hyphenated uuid, and os-release is a
// dozen shell assignments of which exactly one matters.
func staged() (fstest.MapFS, fstest.MapFS) {
	proc := fstest.MapFS{
		"sys/kernel/hostname":        {Data: []byte("host-01\n")},
		"sys/kernel/osrelease":       {Data: []byte("6.1.0-18-amd64\n")},
		"sys/kernel/random/boot_id":  {Data: []byte("9f2c1e40-6b1a-4c77-8f39-2a5f0c1d8e43\n")},
		"sys/kernel/random/entropy":  {Data: []byte("256\n")},
		"sys/kernel/osrelease.stale": {Data: []byte("5.10.0-old\n")},
	}
	etc := fstest.MapFS{
		"os-release": {Data: []byte(strings.Join([]string{
			`PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"`,
			`NAME="Debian GNU/Linux"`,
			`VERSION_ID="12"`,
			`ID=debian`,
			`HOME_URL="https://www.debian.org/"`,
			"",
		}, "\n"))},
	}
	return proc, etc
}

func TestFromReadsEveryFieldOffTheStagedTree(t *testing.T) {
	proc, etc := staged()
	want := report.Host{
		Hostname: "host-01",
		OS:       "linux",
		Arch:     "aarch64",
		Kernel:   "6.1.0-18-amd64",
		Distro:   "Debian GNU/Linux 12 (bookworm)",
		BootID:   "9f2c1e40-6b1a-4c77-8f39-2a5f0c1d8e43",
	}
	if got := From(proc, etc, "aarch64"); got != want {
		t.Errorf("From()\n got %+v\nwant %+v", got, want)
	}
}

// Five of the six fields are strings read out of files, so a transposed pair
// compiles and reports the kernel as the hostname on every host forever. Each
// file is dropped in turn and exactly one field must go with it.
func TestEachFileFeedsExactlyOneField(t *testing.T) {
	proc, etc := staged()
	full := From(proc, etc, arch)
	for file, want := range map[string]string{
		hostnamePath: "Hostname",
		kernelPath:   "Kernel",
		bootIDPath:   "BootID",
	} {
		t.Run(file, func(t *testing.T) {
			proc, etc := staged()
			delete(proc, file)
			got := differing(full, From(proc, etc, arch))
			if len(got) != 1 || got[0] != want {
				t.Errorf("dropping %s changed %v, want exactly [%s]", file, got, want)
			}
		})
	}

	t.Run(osReleasePath, func(t *testing.T) {
		proc, etc := staged()
		delete(etc, osReleasePath)
		got := differing(full, From(proc, etc, arch))
		if len(got) != 1 || got[0] != "Distro" {
			t.Errorf("dropping %s changed %v, want exactly [Distro]", osReleasePath, got)
		}
	})
}

// differing names the report.Host fields that a and b disagree about, so a
// failure says which field moved rather than printing two structs to diff by eye.
func differing(a, b report.Host) []string {
	var names []string
	ta, va, vb := reflect.TypeOf(a), reflect.ValueOf(a), reflect.ValueOf(b)
	for i := range ta.NumField() {
		if va.Field(i).String() != vb.Field(i).String() {
			names = append(names, ta.Field(i).Name)
		}
	}
	return names
}

// A host that will not say what it is gets an empty distro. v1 wrote the literal
// string "Unknown Linux" here, which a consumer grouping by distro counts as a
// distro -- one that appears on every host with an unreadable /etc.
func TestAnUnreadableHostIsEmptyAndNotAPlaceholder(t *testing.T) {
	got := From(fstest.MapFS{}, fstest.MapFS{}, "x86_64")
	want := report.Host{OS: "linux", Arch: "x86_64"}
	if got != want {
		t.Errorf("From() on an empty tree\n got %+v\nwant %+v", got, want)
	}
	for name, v := range map[string]string{
		"hostname": got.Hostname, "kernel": got.Kernel,
		"distro": got.Distro, "boot_id": got.BootID,
	} {
		if strings.Contains(strings.ToLower(v), "unknown") {
			t.Errorf("%s was guessed at: %q", name, v)
		}
	}
}

// OS is a constant rather than runtime.GOOS: this binary does not build for
// anything else, and a field that could only ever say one thing should say it
// without a package-level dependency on the toolchain's opinion.
func TestOSIsLinuxAndArchIsWhateverItWasHanded(t *testing.T) {
	got := From(fstest.MapFS{}, fstest.MapFS{}, "riscv64")
	if got.OS != "linux" {
		t.Errorf("os = %q", got.OS)
	}
	if got.Arch != "riscv64" {
		t.Errorf("arch = %q, so From is not passing the machine name through", got.Arch)
	}
}

func TestPrettyName(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want string
	}{
		"double quoted": {`PRETTY_NAME="Debian GNU/Linux 12 (bookworm)"`, "Debian GNU/Linux 12 (bookworm)"},
		"single quoted": {`PRETTY_NAME='Alpine Linux v3.19'`, "Alpine Linux v3.19"},
		"bare":          {`PRETTY_NAME=Buildroot`, "Buildroot"},
		"empty quoted":  {`PRETTY_NAME=""`, ""},
		"empty bare":    {`PRETTY_NAME=`, ""},
		// The line that matters is rarely the first one.
		"after other keys":  {"ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\n", "Ubuntu 24.04.1 LTS"},
		"before other keys": {"PRETTY_NAME=\"Ubuntu 24.04.1 LTS\"\nID=ubuntu\n", "Ubuntu 24.04.1 LTS"},
		// A key that ends in PRETTY_NAME is a different key. Matching on a
		// substring would read the wrong one and never say so.
		"a key that merely ends in it":     {"UBUNTU_PRETTY_NAME=\"nope\"\n", ""},
		"a key that merely starts with it": {"PRETTY_NAME_LONG=\"nope\"\n", ""},
		"no pretty name":                   {"ID=debian\nNAME=\"Debian GNU/Linux\"\n", ""},
		"nothing at all":                   {"", ""},
		"a comment and blank lines":        {"# os-release\n\nPRETTY_NAME=\"Fedora Linux 40\"\n\n", "Fedora Linux 40"},
		// Leading whitespace is not legal os-release, and a file that has it has
		// still said what it is.
		"indented": {"  PRETTY_NAME=\"Arch Linux\"  \n", "Arch Linux"},
		// A file that came through a Windows editor or a CRLF-preserving copy.
		"crlf": {"ID=debian\r\nPRETTY_NAME=\"Debian GNU/Linux 12\"\r\n", "Debian GNU/Linux 12"},
		// No trailing newline: the last line is still a line.
		"unterminated final line": {"ID=debian\nPRETTY_NAME=\"Debian GNU/Linux 12\"", "Debian GNU/Linux 12"},
		// Only a matched pair comes off. A value with one quote is malformed and
		// is reported as it was written rather than half-repaired.
		"one leading quote":  {`PRETTY_NAME="Debian`, `"Debian`},
		"one trailing quote": {`PRETTY_NAME=Debian"`, `Debian"`},
		"mismatched pair":    {`PRETTY_NAME="Debian'`, `"Debian'`},
		"a lone quote":       {`PRETTY_NAME="`, `"`},
		// Quotes inside the value survive; only the outer pair is structural.
		"inner quotes": {`PRETTY_NAME="Debian "sid""`, `Debian "sid"`},
		// os-release permits backslash escapes in a quoted value. No distro puts
		// one in PRETTY_NAME, and half a shell parser is more to get wrong than
		// the case is worth, so it comes through visibly odd rather than wrong.
		"a backslash escape": {`PRETTY_NAME="Debian \"sid\""`, `Debian \"sid\"`},
		// A line with no '=' is not an assignment and must not be read as one.
		"a line with no equals": {"PRETTY_NAME\nPRETTY_NAME=\"Debian\"\n", "Debian"},
		// The value may itself contain '=' -- Cut takes the first one.
		"an equals in the value": {`PRETTY_NAME="a=b"`, "a=b"},
		// The first assignment wins, which is what a shell sourcing the file
		// would not do -- but a duplicated PRETTY_NAME is a broken file either
		// way, and picking one deterministically is the only useful behaviour.
		"a duplicate key": {"PRETTY_NAME=\"first\"\nPRETTY_NAME=\"second\"\n", "first"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := PrettyName(tc.in); got != tc.want {
				t.Errorf("PrettyName(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// The values under /proc/sys/kernel all carry a trailing newline, and a hostname
// with one on the end is a hostname no consumer will match against anything.
func TestTheTrailingNewlineIsGone(t *testing.T) {
	proc := fstest.MapFS{
		"sys/kernel/hostname":       {Data: []byte("host-01\n")},
		"sys/kernel/osrelease":      {Data: []byte("6.1.0-18-amd64\n")},
		"sys/kernel/random/boot_id": {Data: []byte("9f2c1e40-6b1a-4c77-8f39-2a5f0c1d8e43\n")},
	}
	got := From(proc, fstest.MapFS{}, "x86_64")
	for name, v := range map[string]string{
		"hostname": got.Hostname, "kernel": got.Kernel, "boot_id": got.BootID,
	} {
		if strings.TrimSpace(v) != v {
			t.Errorf("%s is not trimmed: %q", name, v)
		}
	}
}

// A file that exists and cannot be read is the same answer as one that is not
// there. Reporting nothing beats refusing to produce a document.
func TestAFileThatRefusesToBeReadIsEmpty(t *testing.T) {
	proc := fstest.MapFS{
		// A directory where a file is expected: os.Open succeeds and the read
		// fails, which is a different path through hostfs than an absent file.
		"sys/kernel/hostname/somefile": {Data: []byte("x")},
		"sys/kernel/osrelease":         {Data: []byte("6.1.0-18-amd64\n")},
	}
	etc := fstest.MapFS{"os-release/somefile": {Data: []byte("x")}}
	got := From(proc, etc, "x86_64")
	if got.Hostname != "" {
		t.Errorf("hostname = %q, want empty", got.Hostname)
	}
	if got.Distro != "" {
		t.Errorf("distro = %q, want empty", got.Distro)
	}
	// The rest of the tree is still read.
	if got.Kernel != "6.1.0-18-amd64" {
		t.Errorf("one unreadable file took the kernel with it: %q", got.Kernel)
	}
}

// The kernel's answer, not runtime.GOARCH: those disagree exactly when a binary
// runs under emulation, which is the case where the difference matters. Asserted
// as a shape rather than a value, because the test runs on both arches.
func TestMachineIsTheKernelsName(t *testing.T) {
	got := Machine()
	if got == "" {
		t.Fatal("uname returned nothing")
	}
	if strings.ContainsRune(got, 0) {
		t.Errorf("machine = %q, so the fixed-size buffer's padding came through", got)
	}
	if strings.TrimSpace(got) != got {
		t.Errorf("machine = %q", got)
	}
}

func TestNullTerminated(t *testing.T) {
	for name, tc := range map[string]struct {
		in   []byte
		want string
	}{
		"padded":         {append([]byte("x86_64"), make([]byte, 59)...), "x86_64"},
		"exactly filled": {[]byte("aarch64"), "aarch64"},
		"empty":          {[]byte{0}, ""},
		"nothing at all": {nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := nullTerminated(tc.in); got != tc.want {
				t.Errorf("nullTerminated(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Detect reads the machine this test is running on. There is nothing to compare
// it against, so it asserts only what is true of every Linux host -- and that it
// returns at all, which is the whole contract: no error, no panic, no refusal.
func TestDetectReadsTheLiveHost(t *testing.T) {
	got := Detect()
	if got.OS != "linux" {
		t.Errorf("os = %q", got.OS)
	}
	if got.Arch == "" {
		t.Error("arch is empty on a live host")
	}
	if got.Hostname == "" {
		t.Error("hostname is empty; /proc/sys/kernel/hostname is readable on every Linux host")
	}
	if got.Kernel == "" {
		t.Error("kernel is empty; /proc/sys/kernel/osrelease is readable on every Linux host")
	}
	if got.Arch != Machine() {
		t.Errorf("Detect reported arch %q and Machine says %q", got.Arch, Machine())
	}
}
