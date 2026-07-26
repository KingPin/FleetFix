package network

import (
	"encoding/json"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Expectations measured against the v1 readers, as elsewhere in this package.

const procFile = "net/route"

// procFS is the one-file /proc these readers are pointed at. fstest.MapFS is the
// whole fixture: no temp directory, and no /proc on the CI host to depend on.
func procFS(text string) fs.FS {
	return fstest.MapFS{procFile: &fstest.MapFile{Data: []byte(text)}}
}

func TestDefaultRouteFixtures(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		iface   string
		gateway string
		ok      bool
	}{
		{
			name: "with a default route", file: "proc/net/route_with_default.txt",
			iface: "eth0", gateway: "192.168.2.1", ok: true,
		},
		{name: "without one", file: "proc/net/route_no_default.txt"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iface, gateway, ok := DefaultRoute(procFS(fixture.Text(t, tt.file)), procFile)
			if iface != tt.iface || gateway != tt.gateway || ok != tt.ok {
				t.Errorf("DefaultRoute() = (%q, %q, %v), want (%q, %q, %v)",
					iface, gateway, ok, tt.iface, tt.gateway, tt.ok)
			}
		})
	}
}

func TestDefaultRouteRows(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		iface   string
		gateway string
		ok      bool
	}{
		{name: "empty", text: ""},
		{
			name: "header only",
			text: "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\tMTU\tWindow\tIRTT\n",
		},
		{
			name:  "plain default",
			text:  "eth0\t00000000\t0100A8C0\t0003\t0\t0\t0\t00000000\t0\t0\t0",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{
			name: "no default row",
			text: "eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0",
		},
		{
			name: "the default is the second row",
			text: "eth0\t0000A8C0\t00000000\t0001\t0\t0\t0\n" +
				"wlan0\t00000000\t0100A8C0\t0003\t0\t0\t0",
			iface: "wlan0", gateway: "192.168.0.1", ok: true,
		},
		{
			name: "the first default wins",
			text: "eth0\t00000000\t0100A8C0\t0003\t0\t0\t0\n" +
				"wlan0\t00000000\t0101A8C0\t0003\t0\t0\t0",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{name: "two fields", text: "eth0\t00000000"},
		{
			name:  "exactly three fields",
			text:  "eth0\t00000000\t0100A8C0",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{name: "a row whose iface is literally Iface", text: "Iface\t00000000\t0100A8C0"},
		{
			// The skip is an exact match, so the lowercase spelling is data.
			name:  "iface in lowercase is not the header",
			text:  "iface\t00000000\t0100A8C0",
			iface: "iface", gateway: "192.168.0.1", ok: true,
		},
		{
			name:  "a zero gateway is still a default route",
			text:  "eth0\t00000000\t00000000\t0003",
			iface: "eth0", gateway: "0.0.0.0", ok: true,
		},
		{name: "odd length gateway", text: "eth0\t00000000\t0100A8C\t0003"},
		{
			// No length check: two hex characters make a one-component "address".
			name:  "single byte gateway",
			text:  "eth0\t00000000\tFF\t0003",
			iface: "eth0", gateway: "255", ok: true,
		},
		{
			name:  "two byte gateway",
			text:  "eth0\t00000000\t0102\t0003",
			iface: "eth0", gateway: "2.1", ok: true,
		},
		{
			name:  "five byte gateway",
			text:  "eth0\t00000000\t0100A8C000\t0003",
			iface: "eth0", gateway: "0.192.168.0.1", ok: true,
		},
		{
			// The empty field vanishes in the split, so Flags is read as the gateway.
			name:  "empty gateway field",
			text:  "eth0\t00000000\t\t0003",
			iface: "eth0", gateway: "3.0", ok: true,
		},
		{name: "non-hex gateway", text: "eth0\t00000000\tZZZZZZZZ\t0003"},
		{name: "0x-prefixed gateway", text: "eth0\t00000000\t0x0100A8C0"},
		{
			// bytes.fromhex is ASCII-only, unlike int().
			name: "unicode digits in the gateway", text: "eth0\t00000000\t٠١٠٠A8C0\t0003",
		},
		{
			name:  "lowercase hex",
			text:  "eth0\t00000000\t0100a8c0\t0003",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{
			// The return is from inside the loop, so the later good row is never seen.
			name: "a malformed default hides the one below it",
			text: "eth0\t00000000\tZZ\t0003\n" +
				"wlan0\t00000000\t0100A8C0\t0003",
		},
		{
			name:  "spaces instead of tabs",
			text:  "eth0 00000000 0100A8C0 0003",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{
			name:  "leading whitespace",
			text:  "   eth0\t00000000\t0100A8C0",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{
			name:  "blank lines",
			text:  "\n\neth0\t00000000\t0100A8C0\n\n",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{
			// Python's splitlines boundaries, not just \n.
			name:  "exotic line boundary",
			text:  "junk\x0ceth0\t00000000\t0100A8C0",
			iface: "eth0", gateway: "192.168.0.1", ok: true,
		},
		{
			// The destination test is a string comparison, not a numeric one.
			name: "a destination of 0 is not 00000000", text: "eth0\t0\t0100A8C0",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			iface, gateway, ok := DefaultRoute(procFS(tt.text), procFile)
			if iface != tt.iface || gateway != tt.gateway || ok != tt.ok {
				t.Errorf("DefaultRoute() = (%q, %q, %v), want (%q, %q, %v)",
					iface, gateway, ok, tt.iface, tt.gateway, tt.ok)
			}
			if !ok && (iface != "" || gateway != "") {
				t.Errorf("DefaultRoute() returned (%q, %q) alongside ok=false", iface, gateway)
			}
		})
	}
}

func TestDefaultRouteOnAnUnreadableFile(t *testing.T) {
	if _, _, ok := DefaultRoute(fstest.MapFS{}, procFile); ok {
		t.Error("DefaultRoute() reported a route from a /proc with no route file")
	}
}

func TestReadCountersFixtures(t *testing.T) {
	got := ReadCounters(procFS(fixture.Text(t, "proc/net/dev.txt")), procFile)
	want := map[string]Counters{
		"lo":   {RxBytes: 1234567, TxBytes: 1234567},
		"eth0": {RxBytes: 9876543, TxBytes: 5555555},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ReadCounters() mismatch (-want +got):\n%s", diff)
	}
	checkCounters(t, got)
}

func TestReadCountersRows(t *testing.T) {
	// Nine numbers is the minimum row: rx is the first, tx the ninth.
	const nine = " 0 1 2 3 4 5 6 7 8"

	tests := []struct {
		name string
		text string
		want map[string]Counters
	}{
		{name: "empty", text: ""},
		{
			name: "headers carry no colon",
			text: "Inter-|   Receive                                                |  Transmit\n" +
				" face |bytes    packets errs drop fifo frame compressed multicast|" +
				"bytes    packets errs drop fifo colls carrier compressed\n",
		},
		{name: "no colon", text: "eth0 1 2 3 4 5 6 7 8 9"},
		{name: "exactly nine numbers", text: "eth0:" + nine, want: map[string]Counters{"eth0": {TxBytes: 8}}},
		{name: "eight numbers", text: "eth0: 0 1 2 3 4 5 6 7"},
		{
			// Only the ninth is read, so the rest of the row is free to be anything.
			name: "more than nine numbers",
			text: "eth0:" + nine + " 9 10 banana",
			want: map[string]Counters{"eth0": {TxBytes: 8}},
		},
		{
			// The cut is at the first colon, so an alias leaves ":" in the values.
			name: "an alias interface", text: "eth0:1:" + nine,
		},
		{name: "a colon among the values", text: "eth0: 0 1 2 3 4 5 6 7 8:9"},
		{
			// Nothing requires the name to be non-empty.
			name: "empty name", text: ":" + nine,
			want: map[string]Counters{"": {TxBytes: 8}},
		},
		{name: "whitespace name", text: "   :" + nine, want: map[string]Counters{"": {TxBytes: 8}}},
		{
			// Python's strip() is Unicode-aware.
			name: "no-break spaces around the name", text: "\u00a0eth0\u00a0:" + nine,
			want: map[string]Counters{"eth0": {TxBytes: 8}},
		},
		{
			name: "duplicate interface, the last row wins",
			text: "eth0:" + nine + "\neth0: 100 101 102 103 104 105 106 107 108",
			want: map[string]Counters{"eth0": {RxBytes: 100, TxBytes: 108}},
		},
		{name: "non-numeric rx", text: "eth0: banana 1 2 3 4 5 6 7 8"},
		{name: "non-numeric tx", text: "eth0: 0 1 2 3 4 5 6 7 banana"},
		{name: "a float rx", text: "eth0: 1.0 1 2 3 4 5 6 7 8"},
		{
			// int() takes a sign, so the kernel's u64 counters are not the only
			// thing that parses.
			name: "negative counters", text: "eth0: -1 1 2 3 4 5 6 7 -8",
			want: map[string]Counters{"eth0": {RxBytes: -1, TxBytes: -8}},
		},
		{
			name: "signed counters", text: "eth0: +1 1 2 3 4 5 6 7 +8",
			want: map[string]Counters{"eth0": {RxBytes: 1, TxBytes: 8}},
		},
		{
			name: "underscore separators", text: "eth0: 1_000 1 2 3 4 5 6 7 2_000",
			want: map[string]Counters{"eth0": {RxBytes: 1000, TxBytes: 2000}},
		},
		{
			// int() is Unicode-aware, unlike bytes.fromhex above.
			name: "unicode digits", text: "eth0: ٤٢ 1 2 3 4 5 6 7 ٧",
			want: map[string]Counters{"eth0": {RxBytes: 42, TxBytes: 7}},
		},
		{name: "tabs between the numbers", text: "eth0:\t0\t1\t2\t3\t4\t5\t6\t7\t8", want: map[string]Counters{"eth0": {TxBytes: 8}}},
		{name: "no space after the colon", text: "eth0:0 1 2 3 4 5 6 7 8", want: map[string]Counters{"eth0": {TxBytes: 8}}},
		{
			// Python's splitlines boundaries, not just \n.
			name: "exotic line boundary",
			text: "eth0:" + nine + "\u001deth1: 100 101 102 103 104 105 106 107 108",
			want: map[string]Counters{"eth0": {TxBytes: 8}, "eth1": {RxBytes: 100, TxBytes: 108}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := tt.want
			if want == nil {
				want = map[string]Counters{}
			}
			got := ReadCounters(procFS(tt.text), procFile)
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("ReadCounters() mismatch (-want +got):\n%s", diff)
			}
			checkCounters(t, got)
		})
	}
}

func TestReadCountersOnAnUnreadableFile(t *testing.T) {
	got := ReadCounters(fstest.MapFS{}, procFile)
	if len(got) != 0 {
		t.Errorf("ReadCounters() = %v from a /proc with no dev file, want nothing", got)
	}
	checkCounters(t, got)
}

// TestReadCountersHugeCounterDropsTheInterface documents the one departure from
// v1: Python's int() has no width, so it reports a counter no int64 can hold.
// Unreachable from a real /proc/net/dev, whose counters are u64.
func TestReadCountersHugeCounterDropsTheInterface(t *testing.T) {
	huge := strings.Repeat("9", 30)
	got := ReadCounters(procFS("eth0: "+huge+" 1 2 3 4 5 6 7 8"), procFile)
	if len(got) != 0 {
		t.Errorf("ReadCounters() = %v, want the row dropped", got)
	}
}

func TestReadCountersMarshalsNoInterfacesAsAnObject(t *testing.T) {
	b, err := json.Marshal(ReadCounters(procFS(""), procFile))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(b) != "{}" {
		t.Errorf("json.Marshal() = %s, want {}", b)
	}
}

// checkCounters asserts what holds of every result regardless of input.
func checkCounters(t *testing.T, got map[string]Counters) {
	t.Helper()
	if got == nil {
		t.Error("ReadCounters() returned a nil map, want an empty one")
	}
	for iface := range got {
		// The name is everything before the first colon, trimmed.
		if strings.Contains(iface, ":") {
			t.Errorf("interface %q kept a colon", iface)
		}
		if strings.TrimFunc(iface, pytext.IsSpace) != iface {
			t.Errorf("interface %q kept surrounding whitespace", iface)
		}
	}
}

// TestOperstateFixtures walks the one captured /sys/class/net the manifest holds.
//
// Expectations measured by running v1's operstate() against the same tree, so the
// three shapes that all collapse to "unknown" -- no operstate file, a directory
// where the file should be, an interface name that is not there -- are pinned as
// what v1 does rather than as what the fallback looks like it should do.
func TestOperstateFixtures(t *testing.T) {
	fsys := fixture.Tree(t, "net/operstate.json")

	for _, tt := range []struct {
		iface, want string
	}{
		{"eth0", "up"},
		{"eth1", "down"},
		{"wg0", "unknown"},
		// The trailing newline is not the only thing stripped: v1 spells this
		// read_text().strip(), so leading padding and the ASCII separators go too.
		{"lo", "up"},
		{"sep", "up"},
		// An empty file is "", not "unknown". A reader that conflated the two would
		// grade a driver that wrote nothing the same as one that wrote a word it did
		// not recognise, and the ladder's link rung treats "" as passing.
		{"empty", ""},
		{"blank", ""},
		{"nofile", "unknown"},
		{"isdir", "unknown"},
		{"regular", "unknown"},
		{"enp0s31f6", "unknown"},
	} {
		if got := Operstate(fsys, ".", tt.iface); got != tt.want {
			t.Errorf("Operstate(%q) = %q, want %q", tt.iface, got, tt.want)
		}
	}
}

// TestOperstateOnAnEmptyFilesystem covers the shipping root being absent entirely:
// a container with no /sys/class/net at all, which is not an error to report.
func TestOperstateOnAnEmptyFilesystem(t *testing.T) {
	if got := Operstate(fstest.MapFS{}, SysClassNet, "eth0"); got != "unknown" {
		t.Errorf("Operstate() = %q, want %q", got, "unknown")
	}
}

// TestOperstateRejectsAnAbsolutePath pins the hostfs contract at this call site: an
// fs.FS name is relative, and a caller that passed "/sys/class/net" gets "unknown"
// rather than a path error surfacing to an operator as a broken link.
func TestOperstateRejectsAnAbsolutePath(t *testing.T) {
	fsys := fixture.Tree(t, "net/operstate.json")
	if got := Operstate(fsys, "/", "eth0"); got != "unknown" {
		t.Errorf("Operstate() = %q, want %q", got, "unknown")
	}
}

func FuzzDefaultRoute(f *testing.F) {
	for _, name := range []string{
		"proc/net/route_with_default.txt", "proc/net/route_no_default.txt",
	} {
		f.Add(fixture.Text(f, name))
	}

	f.Fuzz(func(t *testing.T, text string) {
		iface, gateway, ok := DefaultRoute(procFS(text), procFile)
		if !ok && (iface != "" || gateway != "") {
			t.Errorf("DefaultRoute() returned (%q, %q) alongside ok=false", iface, gateway)
		}
		if ok && strings.IndexFunc(iface, pytext.IsSpace) >= 0 {
			t.Errorf("iface %q has whitespace in it", iface)
		}
		if ok && strings.Trim(gateway, "0123456789.") != "" {
			t.Errorf("gateway %q is not digits and dots", gateway)
		}
	})
}

func FuzzReadCounters(f *testing.F) {
	f.Add(fixture.Text(f, "proc/net/dev.txt"))

	f.Fuzz(func(t *testing.T, text string) {
		checkCounters(t, ReadCounters(procFS(text), procFile))
	})
}
