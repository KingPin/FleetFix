package network

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// As with the other parsers here, every expectation was produced by running the
// v1 parser on the same input. Several of the address-splitting cases below are
// not what the code reads like it does.

func pidOf(v int64) *int64 { return &v }

// sock is the common shape: a socket with a process behind it.
func sock(addr string, port int64, name string, pid int64) ListeningSocket {
	return ListeningSocket{LocalAddress: addr, LocalPort: port, ProcessName: str(name), PID: pidOf(pid)}
}

func TestParseSSOutputFixtures(t *testing.T) {
	tests := []struct {
		name string
		file string
		want []ListeningSocket
	}{
		{
			// Nothing is listening: an ESTAB row is a connection, not a listener.
			name: "established only", file: "ss/estab_only.txt",
			want: []ListeningSocket{},
		},
		{
			name: "listen without header", file: "ss/listen_no_header.txt",
			want: []ListeningSocket{
				sock("127.0.0.1", 5432, "postgres", 1234),
				sock("0.0.0.0", 22, "sshd", 812),
				sock("0.0.0.0", 80, "nginx", 5050),
				// "[::]:22" -- the brackets come off.
				sock("::", 22, "sshd", 812),
				// The %lo scope suffix stays on the address.
				sock("127.0.0.53%lo", 53, "systemd-resolve", 412),
			},
		},
		{
			name: "listen without users", file: "ss/listen_no_users.txt",
			want: []ListeningSocket{{LocalAddress: "0.0.0.0", LocalPort: 9999}},
		},
		{
			name: "listen with header", file: "ss/listen_with_header.txt",
			want: []ListeningSocket{sock("127.0.0.1", 5432, "postgres", 1234)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSSOutput(fixture.Text(t, tt.file))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseSSOutput() mismatch (-want +got):\n%s", diff)
			}
			checkSockets(t, got)
		})
	}
}

func TestParseSSOutputRows(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []ListeningSocket
	}{
		{name: "empty", text: "", want: []ListeningSocket{}},
		{name: "blank lines only", text: "\n\n   \n", want: []ListeningSocket{}},
		{name: "State header", text: "State Recv-Q Send-Q Local Address:Port", want: []ListeningSocket{}},
		{name: "Netid header", text: "Netid State Recv-Q Send-Q Local", want: []ListeningSocket{}},
		{
			// The header test runs on the stripped line, so indentation does not
			// smuggle a header through.
			name: "indented header", text: "   State Recv-Q Send-Q Local Address:Port",
			want: []ListeningSocket{},
		},
		{
			// startswith, not equality: anything beginning "State" is a header.
			name: "State prefix is enough", text: "Stateful 0 0 1.2.3.4:80",
			want: []ListeningSocket{},
		},
		{name: "too few fields", text: "LISTEN 0 128", want: []ListeningSocket{}},
		{
			name: "four fields is enough", text: "LISTEN 0 128 1.2.3.4:80",
			want: []ListeningSocket{{LocalAddress: "1.2.3.4", LocalPort: 80}},
		},
		{name: "state is case sensitive", text: "listen 0 128 1.2.3.4:80", want: []ListeningSocket{}},
		{name: "ESTAB", text: "ESTAB 0 0 1.2.3.4:443 5.6.7.8:1", want: []ListeningSocket{}},
		{name: "UNCONN", text: "UNCONN 0 0 0.0.0.0:68 0.0.0.0:*", want: []ListeningSocket{}},
		{
			name: "tab separated", text: "LISTEN\t0\t128\t1.2.3.4:80\t0.0.0.0:*",
			want: []ListeningSocket{{LocalAddress: "1.2.3.4", LocalPort: 80}},
		},
		{
			// Python's split() is Unicode-aware, so a no-break space separates
			// fields exactly like a plain one.
			name: "no-break space separated", text: "LISTEN 0 128 1.2.3.4:80",
			want: []ListeningSocket{{LocalAddress: "1.2.3.4", LocalPort: 80}},
		},
		{
			// ...but a form feed is a *line* boundary, so this is five one-field
			// lines rather than one five-field row.
			name: "form feed is a line boundary", text: "LISTEN\x0c0\x0c128\x0c1.2.3.4:80",
			want: []ListeningSocket{},
		},
		{
			name: "CRLF", text: "LISTEN 0 128 1.2.3.4:80\r\nLISTEN 0 128 1.2.3.4:81\r\n",
			want: []ListeningSocket{
				{LocalAddress: "1.2.3.4", LocalPort: 80},
				{LocalAddress: "1.2.3.4", LocalPort: 81},
			},
		},
		{
			// The rest of Python's splitlines boundaries, which Go's Split on "\n"
			// would run together into one unparseable row.
			name: "exotic line boundaries",
			text: "LISTEN 0 128 1.2.3.4:80\x0bLISTEN 0 128 1.2.3.4:81" +
				"\x1cLISTEN 0 128 1.2.3.4:82\u0085LISTEN 0 128 1.2.3.4:83" +
				"\u2028LISTEN 0 128 1.2.3.4:84",
			want: []ListeningSocket{
				{LocalAddress: "1.2.3.4", LocalPort: 80},
				{LocalAddress: "1.2.3.4", LocalPort: 81},
				{LocalAddress: "1.2.3.4", LocalPort: 82},
				{LocalAddress: "1.2.3.4", LocalPort: 83},
				{LocalAddress: "1.2.3.4", LocalPort: 84},
			},
		},
		{name: "no port in local address", text: "LISTEN 0 128 nocolon 0.0.0.0:*", want: []ListeningSocket{}},
		{
			// Not deduplicated: two identical rows are two entries.
			name: "duplicate rows", text: "LISTEN 0 128 1.2.3.4:80\nLISTEN 0 128 1.2.3.4:80",
			want: []ListeningSocket{
				{LocalAddress: "1.2.3.4", LocalPort: 80},
				{LocalAddress: "1.2.3.4", LocalPort: 80},
			},
		},
		{
			// The users pattern is searched against the whole row, not the field
			// ss puts it in, so its position does not matter.
			name: "users field out of position",
			text: `LISTEN ("p",pid=9,fd=1) 128 1.2.3.4:80`,
			want: []ListeningSocket{sock("1.2.3.4", 80, "p", 9)},
		},
		{
			// Only the first entry is read.
			name: "socket shared by two processes",
			text: `LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:(("nginx",pid=1,fd=6),("nginx",pid=2,fd=6))`,
			want: []ListeningSocket{sock("0.0.0.0", 80, "nginx", 1)},
		},
		{
			// [^"]+ needs a character, so an empty process name is no match at
			// all rather than an entry with an empty name.
			name: "empty process name",
			text: `LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:(("",pid=1,fd=1))`,
			want: []ListeningSocket{{LocalAddress: "0.0.0.0", LocalPort: 80}},
		},
		{
			name: "process name with a space",
			text: `LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:(("a b",pid=7,fd=0))`,
			want: []ListeningSocket{sock("0.0.0.0", 80, "a b", 7)},
		},
		{
			// \d is Unicode in Python, and int() reads those digits.
			name: "pid in Arabic-Indic digits",
			text: `LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:(("a",pid=٤٢,fd=0))`,
			want: []ListeningSocket{sock("0.0.0.0", 80, "a", 42)},
		},
		{
			name: "pid with leading zeros",
			text: `LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:(("a",pid=007,fd=0))`,
			want: []ListeningSocket{sock("0.0.0.0", 80, "a", 7)},
		},
		{
			// The pattern has no sign, so a negative pid is not a users entry.
			name: "negative pid",
			text: `LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:(("a",pid=-7,fd=0))`,
			want: []ListeningSocket{{LocalAddress: "0.0.0.0", LocalPort: 80}},
		},
		{
			name: "empty fd",
			text: `LISTEN 0 128 0.0.0.0:80 0.0.0.0:* users:(("a",pid=7,fd=))`,
			want: []ListeningSocket{{LocalAddress: "0.0.0.0", LocalPort: 80}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSSOutput(tt.text)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseSSOutput() mismatch (-want +got):\n%s", diff)
			}
			checkSockets(t, got)
		})
	}
}

func TestSplitAddrPort(t *testing.T) {
	tests := []struct {
		local string
		addr  string
		port  int64
		ok    bool
	}{
		{local: "127.0.0.1:5432", addr: "127.0.0.1", port: 5432, ok: true},
		{local: "[::]:22", addr: "::", port: 22, ok: true},
		{local: "[::1]:22", addr: "::1", port: 22, ok: true},
		{local: "[fe80::1%eth0]:53", addr: "fe80::1%eth0", port: 53, ok: true},
		{local: "127.0.0.53%lo:53", addr: "127.0.0.53%lo", port: 53, ok: true},
		// The bracket branch skips the ']' and one further character without
		// checking that it is a colon.
		{local: "[::1]22", addr: "::1", port: 2, ok: true},
		{local: "[::1]x22", addr: "::1", port: 22, ok: true},
		{local: "[::1]\x0080", addr: "::1", port: 80, ok: true},
		// One character, not one byte.
		{local: "[::1]€22", addr: "::1", port: 22, ok: true},
		{local: "[::1]\u200b22", addr: "::1", port: 22, ok: true},
		{local: "[::1]€", addr: "::1", ok: false},
		{local: "[::1]", addr: "::1", ok: false},
		{local: "[::1]:", addr: "::1", ok: false},
		// An empty bracketed address is an address, and the port still reads.
		{local: "[]:80", addr: "", port: 80, ok: true},
		{local: "[]", addr: "", ok: false},
		// No closing bracket: the whole thing comes back as the address.
		{local: "[abc", addr: "[abc", ok: false},
		{local: "[", addr: "[", ok: false},
		{local: "foo", addr: "foo", ok: false},
		{local: "", addr: "", ok: false},
		// rpartition splits on the *last* colon.
		{local: "a:b:80", addr: "a:b", port: 80, ok: true},
		{local: "::80", addr: ":", port: 80, ok: true},
		// A leading colon leaves the address empty, which v1 treats as "no port
		// here" and returns the input unsplit.
		{local: ":80", addr: ":80", ok: false},
		{local: ":", addr: ":", ok: false},
		{local: "1.2.3.4:", addr: "1.2.3.4", ok: false},
		{local: "1.2.3.4:*", addr: "1.2.3.4", ok: false},
		{local: "*:*", addr: "*", ok: false},
		// int(), not a port parser: sign, underscores and Unicode digits all read,
		// and the result is not range-checked.
		{local: "1.2.3.4:-5", addr: "1.2.3.4", port: -5, ok: true},
		{local: "1.2.3.4:+80", addr: "1.2.3.4", port: 80, ok: true},
		{local: "1.2.3.4:8_0", addr: "1.2.3.4", port: 80, ok: true},
		{local: "1.2.3.4:٤٢", addr: "1.2.3.4", port: 42, ok: true},
		{local: "1.2.3.4:80 ", addr: "1.2.3.4", port: 80, ok: true},
		{local: "1.2.3.4:0x10", addr: "1.2.3.4", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.local, func(t *testing.T) {
			addr, port, ok := splitAddrPort(tt.local)
			if addr != tt.addr || port != tt.port || ok != tt.ok {
				t.Errorf("splitAddrPort(%q) = (%q, %d, %v), want (%q, %d, %v)",
					tt.local, addr, port, ok, tt.addr, tt.port, tt.ok)
			}
		})
	}
}

// A port past int64 is v1's one arbitrary-precision escape here, and the
// documented departure: the socket goes away rather than reporting a wrong port.
func TestParseSSOutputHugePortDropsTheSocket(t *testing.T) {
	got := ParseSSOutput("LISTEN 0 128 1.2.3.4:99999999999999999999999999 x")
	if len(got) != 0 {
		t.Errorf("ParseSSOutput() = %+v, want no sockets", got)
	}
}

// The same for a pid, except the socket survives -- only the process behind it
// is lost, and it is lost whole rather than as a name with no number.
func TestParseSSOutputHugePIDDropsTheProcess(t *testing.T) {
	got := ParseSSOutput(`LISTEN 0 128 1.2.3.4:80 users:(("a",pid=99999999999999999999999999,fd=1))`)
	want := []ListeningSocket{{LocalAddress: "1.2.3.4", LocalPort: 80}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseSSOutput() mismatch (-want +got):\n%s", diff)
	}
}

func TestParseSSOutputMarshalsNoSocketsAsAList(t *testing.T) {
	b, err := json.Marshal(ParseSSOutput(""))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if string(b) != "[]" {
		t.Errorf("json.Marshal() = %s, want []", b)
	}
}

func TestParseSSOutputMarshalsAbsentProcessAsNull(t *testing.T) {
	b, err := json.Marshal(ParseSSOutput("LISTEN 0 128 1.2.3.4:80"))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	const want = `[{"local_address":"1.2.3.4","local_port":80,"process_name":null,"pid":null}]`
	if string(b) != want {
		t.Errorf("json.Marshal() = %s, want %s", b, want)
	}
}

// checkSockets asserts what holds of every result regardless of input, so the
// fuzz target and the table tests police the same invariants.
func checkSockets(t *testing.T, got []ListeningSocket) {
	t.Helper()
	if got == nil {
		t.Error("ParseSSOutput() returned a nil slice, want an empty one")
	}
	for _, s := range got {
		if (s.ProcessName == nil) != (s.PID == nil) {
			t.Errorf("socket %+v has half a users entry", s)
		}
		if s.ProcessName != nil {
			if *s.ProcessName == "" {
				t.Errorf("socket %+v has an empty process name", s)
			}
			if strings.Contains(*s.ProcessName, `"`) {
				t.Errorf("socket %+v has a quote in its process name", s)
			}
		}
		// The address comes out of a whitespace-split field, so it cannot
		// contain whitespace however mangled the row was.
		if strings.IndexFunc(s.LocalAddress, pytext.IsSpace) >= 0 {
			t.Errorf("socket %+v has whitespace in its address", s)
		}
	}
}

func FuzzParseSSOutput(f *testing.F) {
	for _, name := range []string{
		"ss/estab_only.txt", "ss/listen_no_header.txt",
		"ss/listen_no_users.txt", "ss/listen_with_header.txt",
	} {
		f.Add(fixture.Text(f, name))
	}
	f.Add("LISTEN 0 128 [::1]x22 0.0.0.0:*")
	f.Add(`LISTEN 0 128 1.2.3.4:-5 users:(("a b",pid=7,fd=0))`)

	f.Fuzz(func(t *testing.T, output string) {
		checkSockets(t, ParseSSOutput(output))
	})
}
