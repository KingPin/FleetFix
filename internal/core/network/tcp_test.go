package network

import (
	"encoding/json"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// As with the other parsers here, every expectation below was produced by running
// the v1 parser on the same input.

// dp names a default port for a test case, since the parameter is a pointer and a
// literal has no address.
func dp(p int64) *int64 { return &p }

func TestParseHostPortFixture(t *testing.T) {
	// One target per line, the shape an operator's probes.yml holds. Every line is
	// a target that resolves without a default, so the fixture doubles as the
	// differential case: the harness parses each line with no default_port.
	for _, line := range fixtureTargetLines(t, "tcp/targets.txt") {
		got, ok := ParseHostPort(line, nil)
		if !ok {
			t.Errorf("ParseHostPort(%q) = not a target, want one", line)
			continue
		}
		if got.Host == "" || got.Port < 1 {
			t.Errorf("ParseHostPort(%q) = %+v, want a host and a real port", line, got)
		}
	}
}

func TestParseHostPortBareHostsFixture(t *testing.T) {
	// The other half of the corpus: lines that need a default port to name a
	// target at all, mixed with lines no default can rescue. Read with 5432, the
	// default an operator's probes.yml would carry for a database probe.
	want := []struct {
		host string
		port int64
		ok   bool
	}{
		{"db.internal", 5432, true},
		{"fe80::1", 5432, true},
		{"::1", 5432, true},
		// An unknown scheme leaves the port unset, so the default still applies.
		{"files.example.com", 5432, true},
		{"db.internal", 5433, true},
		// Two colons is the whole string as the host, default port and all.
		{"h:443:8", 5432, true},
		{"", 0, false}, // no host
		{"", 0, false}, // empty brackets
		{"", 0, false}, // unclosed bracket
		{"", 0, false}, // a bad explicit port is fatal, not a fall back to 5432
	}

	lines := fixtureTargetLines(t, "tcp/bare_hosts.txt")
	if len(lines) != len(want) {
		t.Fatalf("tcp/bare_hosts.txt has %d lines, want %d", len(lines), len(want))
	}
	for i, line := range lines {
		got, ok := ParseHostPort(line, dp(5432))
		if ok != want[i].ok || got.Host != want[i].host || got.Port != want[i].port {
			t.Errorf("ParseHostPort(%q, 5432) = %+v, %v; want {%s %d}, %v",
				line, got, ok, want[i].host, want[i].port, want[i].ok)
		}
	}
}

func fixtureTargetLines(tb testing.TB, rel string) []string {
	tb.Helper()
	var out []string
	for _, line := range pytext.SplitLines(fixture.Text(tb, rel)) {
		if line != "" {
			out = append(out, line)
		}
	}
	if len(out) == 0 {
		tb.Fatalf("%s has no target lines", rel)
	}
	return out
}

func TestParseHostPort(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		defaultPort *int64
		want        TCPTarget
		wantOK      bool
	}{
		// The ordinary forms.
		{"host and port", "example.com:443", nil, TCPTarget{Host: "example.com", Port: 443}, true},
		{"bare host has no port", "example.com", nil, TCPTarget{}, false},
		{"bare host takes the default", "example.com", dp(80), TCPTarget{Host: "example.com", Port: 80}, true},
		{"surrounding space is stripped", "  example.com:443  ", nil, TCPTarget{Host: "example.com", Port: 443}, true},
		{"empty", "", nil, TCPTarget{}, false},
		{"all space", "   ", nil, TCPTarget{}, false},
		{"space with a default is still nothing", "   ", dp(80), TCPTarget{}, false},

		// IPv6, bracketed and bare.
		{"bracketed with a port", "[::1]:5432", nil, TCPTarget{Host: "::1", Port: 5432}, true},
		{"bracketed without a port", "[::1]", nil, TCPTarget{}, false},
		{"bracketed takes the default", "[::1]", dp(22), TCPTarget{Host: "::1", Port: 22}, true},
		{"bare v6 is all host", "::1", nil, TCPTarget{}, false},
		{"bare v6 takes the default", "::1", dp(22), TCPTarget{Host: "::1", Port: 22}, true},
		{"empty brackets", "[]:443", nil, TCPTarget{}, false},
		{"unclosed bracket", "[::1", nil, TCPTarget{}, false},
		{"bracket then empty port", "[::1]:", nil, TCPTarget{}, false},
		// Junk after the bracket is neither a port nor an error: it is ignored, and
		// the default still applies. v1 does the same.
		{"junk after the bracket", "[::1]x", nil, TCPTarget{}, false},
		{"junk after the bracket with a default", "[::1]x", dp(9), TCPTarget{Host: "::1", Port: 9}, true},
		{"junk hiding a port after the bracket", "[::1]x:9", nil, TCPTarget{}, false},

		// Schemes.
		{"https", "https://example.com", nil, TCPTarget{Host: "example.com", Port: 443}, true},
		{"http", "http://example.com", nil, TCPTarget{Host: "example.com", Port: 80}, true},
		{"ssh", "ssh://h", nil, TCPTarget{Host: "h", Port: 22}, true},
		{"postgres", "postgres://db", nil, TCPTarget{Host: "db", Port: 5432}, true},
		{"postgresql", "postgresql://db", nil, TCPTarget{Host: "db", Port: 5432}, true},
		{"redis", "redis://r", nil, TCPTarget{Host: "r", Port: 6379}, true},
		{"mysql", "mysql://m", nil, TCPTarget{Host: "m", Port: 3306}, true},
		// The scheme is lowered for the lookup; the host is not touched.
		{"scheme is case-insensitive", "HTTPS://example.com", nil, TCPTarget{Host: "example.com", Port: 443}, true},
		{"host case is preserved", "HTTP://H", nil, TCPTarget{Host: "H", Port: 80}, true},
		{"empty scheme", "://h:80", nil, TCPTarget{Host: "h", Port: 80}, true},
		{"unknown scheme", "ftp://h", nil, TCPTarget{}, false},
		{"unknown scheme takes the default", "ftp://h", dp(21), TCPTarget{Host: "h", Port: 21}, true},
		{"scheme with no authority", "http://", nil, TCPTarget{}, false},
		{"scheme with only a path", "http:///path", nil, TCPTarget{}, false},

		// Path and port interaction.
		{"path is dropped", "https://example.com/path/x", nil, TCPTarget{Host: "example.com", Port: 443}, true},
		{"explicit port beats the scheme", "https://example.com:8080/x", nil, TCPTarget{Host: "example.com", Port: 8080}, true},
		{"scheme and bracketed port", "https://[::1]:8080/x", nil, TCPTarget{Host: "::1", Port: 8080}, true},
		{"scheme port for a bracketed host", "https://[::1]/x", nil, TCPTarget{Host: "::1", Port: 443}, true},
		// A bad explicit port is fatal even though the scheme already supplied one.
		{"bad explicit port beats the scheme too", "https://h:0/x", nil, TCPTarget{}, false},
		{"two colons keep the scheme port", "https://h:443:8/x", nil, TCPTarget{Host: "h:443:8", Port: 443}, true},

		// The port is read with Python's int(), then range-checked.
		{"padded port", "h: 443 ", nil, TCPTarget{Host: "h", Port: 443}, true},
		{"signed port", "h:+443", nil, TCPTarget{Host: "h", Port: 443}, true},
		{"underscored port", "h:4_43", nil, TCPTarget{Host: "h", Port: 443}, true},
		{"arabic-indic digit port", "h:\u0663", nil, TCPTarget{Host: "h", Port: 3}, true},
		{"port zero", "h:0", nil, TCPTarget{}, false},
		{"port past the ceiling", "h:65536", nil, TCPTarget{}, false},
		{"port at the ceiling", "h:65535", nil, TCPTarget{Host: "h", Port: 65535}, true},
		{"port at the floor", "h:1", nil, TCPTarget{Host: "h", Port: 1}, true},
		{"negative port", "h:-1", nil, TCPTarget{}, false},
		{"non-numeric port", "h:abc", nil, TCPTarget{}, false},
		{"float port", "h:443.0", nil, TCPTarget{}, false},
		{"hex port", "h:0x1bb", nil, TCPTarget{}, false},
		{"empty port", "h:", nil, TCPTarget{}, false},
		{"no host", ":443", nil, TCPTarget{}, false},
		// An explicit port wins over the default, including a bad one -- the bad
		// port is fatal rather than falling back.
		{"explicit beats the default", "h:443", dp(99), TCPTarget{Host: "h", Port: 443}, true},
		{"bad explicit beats the default", "h:0", dp(99), TCPTarget{}, false},

		// Two colons: no way to tell host from port, so the whole string is the host.
		{"two colons is all host", "h:443:8", nil, TCPTarget{}, false},
		{"two colons with a default", "h:443:8", dp(99), TCPTarget{Host: "h:443:8", Port: 99}, true},

		// Neither userinfo nor a query string is stripped: the host is reported as
		// written and the lookup fails, which is the honest answer.
		{"userinfo is kept", "user@h:22", nil, TCPTarget{Host: "user@h", Port: 22}, true},
		{"query string is kept", "https://h?q=1", nil, TCPTarget{Host: "h?q=1", Port: 443}, true},

		// v1 validates a parsed port and never validates the default. Both
		// reproduced: a caller passing a bad default is the bug, not this.
		{"default zero is not validated", "h", dp(0), TCPTarget{Host: "h", Port: 0}, true},
		{"negative default is not validated", "h", dp(-5), TCPTarget{Host: "h", Port: -5}, true},
		{"oversized default is not validated", "h", dp(999999), TCPTarget{Host: "h", Port: 999999}, true},
		{"bracketed host with a zero default", "[::1]", dp(0), TCPTarget{Host: "::1", Port: 0}, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseHostPort(tc.raw, tc.defaultPort)
			if ok != tc.wantOK || got != tc.want {
				t.Errorf("ParseHostPort(%q, %v) = %+v, %v; want %+v, %v",
					tc.raw, tc.defaultPort, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}

func TestParseHostPortLeavesTheDefaultPortAlone(t *testing.T) {
	// The pointer is read, never written through: a caller reusing one default
	// across a probe list must not have it rewritten by a target that carries its
	// own port.
	def := int64(80)
	if _, ok := ParseHostPort("h:443", &def); !ok {
		t.Fatal("ParseHostPort(h:443) = not a target")
	}
	if def != 80 {
		t.Errorf("default port was mutated to %d, want 80", def)
	}
}

func TestParseHostPortPortAboveInt64(t *testing.T) {
	// pytext.Int reports a range error where Python reads an arbitrary-precision
	// integer, and both answers land on the same rejection: v1's range check
	// rejects it one line later. Not a divergence, but pinned so a future change
	// to pytext.Int cannot quietly turn it into a parse.
	if _, ok := ParseHostPort("h:99999999999999999999", nil); ok {
		t.Error("a port past int64 was accepted")
	}
}

func TestTCPTargetString(t *testing.T) {
	cases := []struct {
		target TCPTarget
		want   string
	}{
		{TCPTarget{Host: "h", Port: 80}, "h:80"},
		{TCPTarget{Host: "example.com", Port: 443}, "example.com:443"},
		// A host containing a colon is bracketed, which is what makes the rendering
		// round-trip back through the parser.
		{TCPTarget{Host: "::1", Port: 443}, "[::1]:443"},
		{TCPTarget{Host: "fe80::1", Port: 22}, "[fe80::1]:22"},
		{TCPTarget{Host: "", Port: 1}, ":1"},
	}
	for _, tc := range cases {
		if got := tc.target.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.target, got, tc.want)
		}
	}
}

func TestTCPTargetStringRoundTrips(t *testing.T) {
	for _, raw := range []string{"h:80", "example.com:443", "[::1]:443", "[fe80::1]:22"} {
		first, ok := ParseHostPort(raw, nil)
		if !ok {
			t.Errorf("ParseHostPort(%q) = not a target", raw)
			continue
		}
		second, ok := ParseHostPort(first.String(), nil)
		if !ok || second != first {
			t.Errorf("%q -> %+v -> %q -> %+v, %v; want the same target", raw, first, first.String(), second, ok)
		}
	}
}

// isResolvableAlphabet reports whether a host is made only of characters a DNS
// lookup or an inet_pton could return: it is the scope of String's round-trip.
//
// Everything outside it -- brackets, whitespace, "/", "://" -- is read
// structurally by the parser, so a target holding one renders to a string that
// means something else. See TestTCPTargetStringIsNotAnEscapingRoundTrip.
func isResolvableAlphabet(host string) bool {
	if host == "" {
		return false
	}
	for _, r := range host {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.', r == '-', r == '_', r == ':', r == '@':
		default:
			return false
		}
	}
	return true
}

func TestTCPTargetStringIsNotAnEscapingRoundTrip(t *testing.T) {
	// The host shapes String cannot render re-readably. All three were found by
	// FuzzParseHostPort and all three measured against v1, which produces the same
	// string and the same rejection -- so this is the contract, not a gap to close.
	// None of them can come out of a DNS lookup or an inet_pton, which is why
	// String stays a rendering rather than growing an escape.
	cases := []struct {
		name   string
		target TCPTarget
		want   string
	}{
		// A "]" in the host closes the bracket early.
		{"closing bracket in the host", TCPTarget{Host: ":]:0]", Port: 55}, "[:]:0]]:55"},
		// An opening bracket makes the rendering look like a v6 literal that never
		// closes. Reachable: parse_host_port("[[]", default_port=1) is this target.
		{"opening bracket is the host", TCPTarget{Host: "[", Port: 1}, "[:1"},
		// Outer whitespace is stripped off the whole string before anything else
		// happens, so a host that is a space cannot survive its own rendering. It
		// gets there through a URL: only raw is stripped, never the authority.
		{"space host", TCPTarget{Host: " ", Port: 50}, " :50"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.target.String(); got != tc.want {
				t.Errorf("String() = %q, want %q", got, tc.want)
			}
			if _, ok := ParseHostPort(tc.target.String(), nil); ok {
				t.Error("the rendering parsed back, want a rejection")
			}
		})
	}
}

func TestParseHostPortStripsOnlyTheWholeString(t *testing.T) {
	// How the two odd hosts above are reachable at all. Both measured against v1.
	//
	// A space host: the authority comes out of a partition and is never stripped
	// on its own, so an unknown scheme carrying one keeps it.
	got, ok := ParseHostPort("x:// :50", nil)
	if !ok || got.Host != " " || got.Port != 50 {
		t.Errorf(`ParseHostPort("x:// :50") = %+v, %v; want host " " port 50`, got, ok)
	}
	// A bracket host: the brackets are stripped off an empty-looking literal whose
	// contents happen to be another bracket.
	got, ok = ParseHostPort("[[]", dp(1))
	if !ok || got.Host != "[" || got.Port != 1 {
		t.Errorf(`ParseHostPort("[[]", 1) = %+v, %v; want host "[" port 1`, got, ok)
	}
}

func TestTCPTargetWireShape(t *testing.T) {
	got, err := json.Marshal(TCPTarget{Host: "example.com", Port: 443})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `{"host":"example.com","port":443}`
	if string(got) != want {
		t.Errorf("Marshal = %s, want %s", got, want)
	}
}

func FuzzParseHostPort(f *testing.F) {
	for _, seed := range []string{
		"example.com:443", "[::1]:5432", "https://example.com/x", "::1", "h:0",
		"  h:1  ", "://h:80", "[]:443", "h:443:8", "user@h:22",
	} {
		f.Add(seed, int64(0), false)
	}
	f.Fuzz(func(t *testing.T, raw string, def int64, haveDefault bool) {
		var defaultPort *int64
		if haveDefault {
			defaultPort = &def
		}
		got, ok := ParseHostPort(raw, defaultPort)
		if !ok {
			if got != (TCPTarget{}) {
				t.Errorf("ParseHostPort(%q) = %+v with ok=false, want the zero target", raw, got)
			}
			return
		}
		// A target always names a host. The port is only range-checked when it came
		// from the input; a default is passed through unvalidated by design.
		if got.Host == "" {
			t.Errorf("ParseHostPort(%q) = %+v, want a non-empty host", raw, got)
		}
		if !haveDefault && (got.Port < 1 || got.Port > 65535) {
			t.Errorf("ParseHostPort(%q) = port %d with no default, want 1..65535", raw, got.Port)
		}
		// Rendering a target parses back to itself, which is what makes String
		// usable as a probe key -- for the hosts a lookup could ever return. It is
		// not an escaping round-trip: brackets and whitespace in a host are read
		// structurally on the way back in, so a target holding them renders to
		// something the parser reads differently or rejects. That is v1's behaviour
		// too, measured, and pinned by TestTCPTargetStringIsNotAnEscapingRoundTrip
		// -- so the invariant is scoped to the resolvable alphabet rather than
		// weakened case by case as the fuzzer finds each shape.
		if got.Port >= 1 && got.Port <= 65535 && isResolvableAlphabet(got.Host) {
			if back, backOK := ParseHostPort(got.String(), nil); !backOK || back != got {
				t.Errorf("%+v.String() = %q, which parses to %+v, %v", got, got.String(), back, backOK)
			}
		}
	})
}
