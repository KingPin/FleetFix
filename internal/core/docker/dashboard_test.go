package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
	"github.com/KingPin/FleetFix/v2/internal/pytime"
	"github.com/google/go-cmp/cmp"
)

// Expectations come from src/fleetfix/modules/docker/dashboard.py under CPython
// 3.14.6, fed the same literals, except where a case is marked as a departure.

func TestParsePSJSONLinesFixture(t *testing.T) {
	const name = "docker/ps_json_with_garbage.txt"
	want := []any{
		map[string]any{
			"ID": "abc", "Names": "web", "Image": "nginx",
			"State": "running", "Status": "Up 2 hours", "Ports": "80/tcp",
		},
		map[string]any{
			"ID": "def", "Names": "db", "Image": "pg",
			"State": "exited", "Status": "Exited (0) 1 hour ago", "Ports": "",
		},
	}
	got := ParsePSJSONLines(fixture.Text(t, name))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParsePSJSONLines(%s) mismatch (-want +got):\n%s", name, diff)
	}
}

func TestParsePSJSONLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []any
	}{
		{"empty", "", []any{}},
		{"one row", `{"ID":"abc","Names":"web"}` + "\n", []any{map[string]any{"ID": "abc", "Names": "web"}}},
		{
			"blank and garbage lines",
			`{"ID":"a"}` + "\n\nnot json\n" + `{"ID":"b"}` + "\n",
			[]any{map[string]any{"ID": "a"}, map[string]any{"ID": "b"}},
		},
		{"indented", `   {"ID":"a"}   ` + "\n", []any{map[string]any{"ID": "a"}}},
		{"no trailing newline", `{"ID":"a"}`, []any{map[string]any{"ID": "a"}}},
		{"crlf", `{"ID":"a"}` + "\r\n", []any{map[string]any{"ID": "a"}}},

		// json.loads rejects a second value on the line.
		{"trailing data", `{"ID":"a"} {"ID":"b"}` + "\n", []any{}},
		{"duplicate keys", `{"ID":"a","ID":"b"}` + "\n", []any{map[string]any{"ID": "b"}}},

		// Numbers keep their literal spelling rather than becoming float64s,
		// because Python's decoder gives an exact int for an integer literal and a
		// 20-digit container ID rounded to the nearest float would be a divergence
		// in a value neither parser touched.
		{"big integer", `{"n":12345678901234567890}` + "\n", []any{map[string]any{"n": json.Number("12345678901234567890")}}},
		{"float", `{"n":1.5}` + "\n", []any{map[string]any{"n": json.Number("1.5")}}},

		{
			"nested",
			`{"a":{"b":[1,{"c":2}]}}` + "\n",
			[]any{map[string]any{"a": map[string]any{"b": []any{json.Number("1"), map[string]any{"c": json.Number("2")}}}}},
		},

		// v1 annotates this list[dict] and then appends whatever json.loads
		// returned, so a line holding any other JSON value is a member too. What
		// crashes is the next function along, not this one.
		{"scalar line", "123\n", []any{json.Number("123")}},
		{"string line", `"hello"` + "\n", []any{"hello"}},
		{"null line", "null\n", []any{nil}},
		{"bool line", "true\n", []any{true}},
		{"array line", "[1,2]\n", []any{[]any{json.Number("1"), json.Number("2")}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePSJSONLines(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParsePSJSONLines(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// TestParsePSJSONLinesDepartsOnTheJSONDialect covers the differences between the
// two decoders, all of them Python accepting or preserving something Go will not,
// and none of them reachable from output a container runtime produces.
func TestParsePSJSONLinesDepartsOnTheJSONDialect(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []any
	}{
		// Python reads these as float literals; Go's decoder rejects the line, so
		// it is dropped the way an unparseable one is.
		{"nan", `{"n":NaN}` + "\n", []any{}},
		{"infinity", `{"n":Infinity}` + "\n", []any{}},
		{"negative infinity", `{"n":-Infinity}` + "\n", []any{}},

		// Python keeps the unpaired surrogate; Go substitutes U+FFFD.
		{"unpaired surrogate", `{"a":"\ud800"}` + "\n", []any{map[string]any{"a": "\ufffd"}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParsePSJSONLines(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParsePSJSONLines(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// TestPairedEscapesStillDecode keeps the surrogate departure above honest: a
// properly paired escape is the same character in both languages, so the
// substitution is confined to input JSON does not allow.
func TestPairedEscapesStillDecode(t *testing.T) {
	got := ParsePSJSONLines(`{"a":"😀","b":"é"}` + "\n")
	want := []any{map[string]any{"a": "\U0001F600", "b": "é"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParsePSJSONLines on paired escapes mismatch (-want +got):\n%s", diff)
	}
}

func TestParsePSJSONLinesReturnsEmptyNotNil(t *testing.T) {
	if got := ParsePSJSONLines(""); got == nil {
		t.Error(`ParsePSJSONLines("") = nil, want an empty slice`)
	}
}

func FuzzParsePSJSONLines(f *testing.F) {
	for _, seed := range []string{
		"",
		`{"ID":"abc","Names":"web","State":"running"}` + "\n",
		`{"ID":"a"}` + "\n\nnot json\n" + `{"ID":"b"}` + "\n",
		"123\nnull\n[1,2]\n",
		`{"n":12345678901234567890}` + "\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		rows := ParsePSJSONLines(in)

		// One row per line at most, so no line can be read twice and none can
		// produce two values -- which is what the trailing-data check in
		// decodeJSONLine is there to guarantee.
		if len(rows) > len(pytext.SplitLines(in)) {
			t.Fatalf("ParsePSJSONLines(%q) produced %d rows from %d lines", in, len(rows), len(pytext.SplitLines(in)))
		}

		// Every row came out of a validating decoder, so re-encoding it cannot
		// fail. A NaN, an infinity or a malformed json.Number leaking through
		// would show up here and nowhere else.
		if _, err := json.Marshal(rows); err != nil {
			t.Fatalf("ParsePSJSONLines(%q) produced a value that will not marshal: %v", in, err)
		}
	})
}

func TestParseInspectFieldsFixture(t *testing.T) {
	const name = "docker/inspect_fields.txt"
	startedAt := "2026-05-16T10:00:00Z"
	want := InspectFields{
		RestartCount: 5,
		LogPath:      "/var/lib/docker/containers/abc/abc-json.log",
		StartedAt:    &startedAt,
		Status:       "running",
	}
	got := ParseInspectFields(fixture.Text(t, name))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseInspectFields(%s) mismatch (-want +got):\n%s", name, diff)
	}
}

func TestParseInspectFields(t *testing.T) {
	s := func(v string) *string { return &v }
	tests := []struct {
		name string
		in   string
		want InspectFields
	}{
		{"all four", "5|/p|2026-05-16T10:00:00Z|running", InspectFields{5, "/p", s("2026-05-16T10:00:00Z"), "running"}},
		{"empty fields", "|||", InspectFields{0, "", s(""), ""}},

		// Too short is the whole line rejected, and started_at reports None rather
		// than the "" an empty third field means.
		{"no delimiters", "garbage", InspectFields{}},
		{"empty", "", InspectFields{}},
		{"whitespace only", "   ", InspectFields{}},
		{"three fields", "5|b|c", InspectFields{}},

		// A LogPath containing a "|" is the reachable case: the extras are dropped
		// and the fields after the second are read from the wrong places. v1 does
		// the same, and neither side can tell which "|" was the separator.
		{"extra fields", "a|b|c|d|e|f", InspectFields{0, "b", s("c"), "d"}},

		// int() on the count, not strconv: whitespace, a sign and underscores are
		// all numbers, and a float or a hex literal is not.
		{"padded count", " 7 |b|c|d", InspectFields{7, "b", s("c"), "d"}},
		{"signed count", "+7|b|c|d", InspectFields{7, "b", s("c"), "d"}},
		{"underscored count", "1_0|b|c|d", InspectFields{10, "b", s("c"), "d"}},
		{"negative count", "-3|b|c|d", InspectFields{-3, "b", s("c"), "d"}},
		{"float count", "7.0|b|c|d", InspectFields{0, "b", s("c"), "d"}},
		{"hex count", "0x10|b|c|d", InspectFields{0, "b", s("c"), "d"}},
		{"empty count", "|b|c|d", InspectFields{0, "b", s("c"), "d"}},
		// Python's int() takes every Unicode Nd digit.
		{"arabic-indic count", "\u0663|b|c|d", InspectFields{3, "b", s("c"), "d"}},

		// Only the whole string is stripped, so the interior padding survives and
		// the fourth field loses only its trailing run.
		{"outer whitespace", "  5|b|c|d  ", InspectFields{5, "b", s("c"), "d"}},
		{"tab and newline", "\t5|b|c|d\n", InspectFields{5, "b", s("c"), "d"}},
		{"interior padding kept", "5|  b  |  c  |  d  ", InspectFields{5, "  b  ", s("  c  "), "  d"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseInspectFields(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseInspectFields(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// The magnitude departure pytext.Int documents, pinned where it is reachable: v1
// keeps an arbitrary-precision count and this lands on zero. Docker's own
// RestartCount is a Go int, so nothing it writes gets here -- which is why this is
// a recorded difference rather than a clamp that would preserve the > 3 comparison
// the caller makes and be wrong about the number.
func TestParseInspectFieldsCountAboveInt64IsZero(t *testing.T) {
	got := ParseInspectFields("99999999999999999999999|b|c|d")
	if got.RestartCount != 0 {
		t.Errorf("RestartCount = %d, want 0", got.RestartCount)
	}
}

// The four keys are always present and in v1's dict order, and started_at is the
// one that has to be able to say null.
func TestInspectFieldsWireShape(t *testing.T) {
	got, err := json.Marshal(ParseInspectFields("5|/p|2026-05-16T10:00:00Z|running"))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"restart_count":5,"log_path":"/p","started_at":"2026-05-16T10:00:00Z","status":"running"}`
	if string(got) != want {
		t.Errorf("marshalled to\n\t%s\nwant\n\t%s", got, want)
	}

	got, err = json.Marshal(ParseInspectFields("garbage"))
	if err != nil {
		t.Fatal(err)
	}
	want = `{"restart_count":0,"log_path":"","started_at":null,"status":""}`
	if string(got) != want {
		t.Errorf("marshalled to\n\t%s\nwant\n\t%s", got, want)
	}
}

func FuzzParseInspectFields(f *testing.F) {
	for _, seed := range []string{
		"5|/p|2026-05-16T10:00:00Z|running",
		"garbage",
		"|||",
		"a|b|c|d|e|f",
		" 7 |b|c|d",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := ParseInspectFields(in)

		// Fewer than four fields is all-or-nothing, so a short line can never
		// report a log path or a status read out of a partial split.
		if len(strings.Split(strings.TrimFunc(in, pytext.IsSpace), "|")) < 4 {
			if got != (InspectFields{}) {
				t.Fatalf("ParseInspectFields(%q) = %#v on a short line, want the zero value", in, got)
			}
			return
		}
		// Past that, every field is present -- started_at is a string, not None.
		if got.StartedAt == nil {
			t.Fatalf("ParseInspectFields(%q) reported no started_at from a full line", in)
		}
		if _, err := json.Marshal(got); err != nil {
			t.Fatalf("ParseInspectFields(%q) produced a value that will not marshal: %v", in, err)
		}
	})
}

// TestDecodeJSONLineRefusesTrailingData reaches the decoder directly, because the
// two parsers above can only show its verdict and not which of the two checks
// produced it.
func TestDecodeJSONLineRefusesTrailingData(t *testing.T) {
	tests := []struct {
		in string
		ok bool
	}{
		{`{"a":1}`, true},
		{`{"a":1} `, true},    // Decode leaves the space; Token then reports EOF.
		{"{\"a\":1}\t", true}, // and any other trailing whitespace
		{`{"a":1} {"b":2}`, false},
		{`{"a":1}]`, false},
		{`{"a":1}x`, false},
		{`1 2`, false},
		{`{"a":`, false},
		{``, false},
		{strings.Repeat("[", 10) + strings.Repeat("]", 10), true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if _, ok := decodeJSONLine(tt.in); ok != tt.ok {
				t.Errorf("decodeJSONLine(%q) ok = %v, want %v", tt.in, ok, tt.ok)
			}
		})
	}
}

// wantStartedAt is v1's answer for each line of docker/started_at.txt, in file
// order, as datetime.isoformat() -- or "" where _parse_iso reported None. Measured
// by running v1's _parse_iso over the fixture, not derived from the ISO grammar.
//
// A line per case, the way procs.parse_stat_comm_and_ticks does it: _parse_iso
// takes a single string, so a case per form would be thirty near-identical
// fixtures and a divergence would name a file rather than a line.
//
// This file holds only the forms every supported interpreter agrees on. The ones
// where 3.11 widened what fromisoformat accepts live in docker/started_at_py311.txt
// -- see wantStartedAtPy311 for why they had to be split out.
var wantStartedAt = []string{
	"2026-07-26T15:04:05+00:00",
	"2026-07-26T15:04:05+00:00",
	"2026-07-26T15:04:05-05:00",
	"2026-07-26T15:04:05", // no offset, so a naive reading -- see pytime.Time
	"2026-07-26T00:00:00",
	"", // docker's zero value: never started
	// A nine-digit fraction, which 3.10 rejects -- but the zero-value guard returns
	// before fromisoformat is reached, so every interpreter answers None here for
	// the same reason. That is what keeps this line on this side of the split.
	"",
	"",                          // the bare date is the prefix too
	"",                          // ... and the prefix is not parsed, so an impossible clock never gets read
	"",                          // ... nor is the rest of the string looked at at all
	"0001-01-02T00:00:00+00:00", // one day past the zero value is a real timestamp
	"0002-01-01T00:00:00+00:00", // and so is one year past it
	// str.replace rewrites every "Z", so a "Z" in the separator slot becomes
	// "+00:00" mid-string and takes a string fromisoformat would have accepted.
	"",
	"",                    // a doubled Z, for the same reason
	"",                    // ... and a Z followed by a real offset
	"",                    // ... and a leading one
	"",                    // fromisoformat wants an uppercase Z, and replace only rewrote uppercase
	"",                    // 2026 is not a leap year
	"2026-07-26T15:04:05", // a space for the separator
	"",
}

// wantStartedAtPy311 is the same measurement for docker/started_at_py311.txt: the
// forms 3.11 added to fromisoformat's accepted language. Every one of these is a
// datetime on 3.11+ and None on 3.10, measured on both interpreters rather than
// read off the changelog.
//
// They are a separate fixture because the differential harness runs its Python
// oracle on 3.10 -- the interpreter the shipped v1.6.0 binary bundles -- so the
// whole case is a known divergence. Folded back into started_at.txt that
// exemption would cover the twenty agreed lines too, and the densest parser
// fixture in the corpus would stop proving anything.
//
// The first line is the one that matters operationally: docker renders
// {{.State.StartedAt}} as RFC3339Nano, so a real container start time always has
// nine fractional digits. On the shipped binary it parses as None, which makes
// Container.is_restart_loop return False for every container -- see
// TestParseISOReadsWhatDockerActuallyWrites.
var wantStartedAtPy311 = []string{
	"2026-07-26T15:04:05.123456+00:00", // what docker actually writes: RFC3339Nano
	"2026-07-26T15:04:05.500000+00:00", // a single fractional digit
	"2026-07-26T15:04:05.123456+05:30",
	"2026-07-27T00:00:00+00:00", // hour 24 is tomorrow's midnight
	"2026-07-26T15:04:05.123456",
	"2026-07-26T15:04:05+00:00",        // an ISO week date
	"2026-07-26T15:04:05+00:00",        // ... and basic format
	"2026-07-26T15:04:05.123456+00:00", // nine fractional digits with a real offset
	"2026-07-26T15:04:05.123456+00:00", // nineteen fractional digits, six kept
}

func TestParseISOFixture(t *testing.T) {
	assertParseISOFixture(t, "docker/started_at.txt", wantStartedAt)
}

func TestParseISOFixturePy311Widenings(t *testing.T) {
	assertParseISOFixture(t, "docker/started_at_py311.txt", wantStartedAtPy311)
}

func assertParseISOFixture(t *testing.T, rel string, want []string) {
	t.Helper()
	lines := fixtureLines(t, rel)
	if len(lines) != len(want) {
		t.Fatalf("fixture has %d lines, the table has %d", len(lines), len(want))
	}
	for i, line := range lines {
		got, ok := ParseISO(line)
		if want := want[i]; want == "" {
			if ok {
				t.Errorf("line %d: ParseISO(%q) = %s, want no answer", i+1, line, got.ISOFormat())
			}
			continue
		} else if !ok {
			t.Errorf("line %d: ParseISO(%q) = no answer, want %s", i+1, line, want)
		} else if s := got.ISOFormat(); s != want {
			t.Errorf("line %d: ParseISO(%q) = %s, want %s", i+1, line, s, want)
		}
	}
}

// TestParseISOReadsWhatDockerActuallyWrites is the regression anchor for the third
// v1.6.0 defect, and the reason the port keeps 3.11+ semantics rather than
// reproducing the interpreter that shipped.
//
// docker renders {{.State.StartedAt}} with Go's RFC3339Nano: nine fractional
// digits and a "Z". CPython 3.10's fromisoformat accepts a fraction of exactly
// three or six digits and nothing else, and the v1.6.0 release binary bundles
// 3.10 -- so on every host running it, _parse_iso returns None for every running
// container. Container.is_restart_loop then short-circuits at its `started_at is
// None` guard and reports False, which means a container in a crash loop is never
// flagged no matter how high its restart count climbs.
//
// Reproducing that would be reproducing a parser that cannot read its own input,
// so this asserts the opposite: the real shape parses, and the restart-loop window
// it feeds is computed from it.
func TestParseISOReadsWhatDockerActuallyWrites(t *testing.T) {
	const rfc3339Nano = "2026-07-26T15:04:05.123456789Z"

	got, ok := ParseISO(rfc3339Nano)
	if !ok {
		t.Fatalf("ParseISO(%q) = no answer; docker writes this shape for every running container", rfc3339Nano)
	}
	if !got.Aware {
		t.Errorf("ParseISO(%q) is naive; is_restart_loop subtracts it from an aware now", rfc3339Nano)
	}
	// Six digits kept, the rest dropped -- Python has microsecond resolution, so
	// the nanoseconds docker offers have nowhere to go.
	if want := "2026-07-26T15:04:05.123456+00:00"; got.ISOFormat() != want {
		t.Errorf("ParseISO(%q) = %s, want %s", rfc3339Nano, got.ISOFormat(), want)
	}
}

// TestParseISOHasNoAnswerForAnEmptyString covers the one case the line-per-case
// fixture cannot hold, because the adapter skips empty lines. v1 tests the value
// for truth, so both the empty string and the None that comes out of a failed
// docker inspect are "never started" -- which is why ParseISO takes a string and
// leaves the *string distinction in InspectFields, where it still means something.
//
// A line of blanks is here too: it is truthy in Python, so it goes to
// fromisoformat and is rejected there instead. Same answer, different route, and
// a fixture cannot carry it without depending on trailing whitespace surviving
// every editor between here and CI.
func TestParseISOHasNoAnswerForAnEmptyString(t *testing.T) {
	for _, in := range []string{"", " ", "\t"} {
		if got, ok := ParseISO(in); ok {
			t.Errorf("ParseISO(%q) = %s, want no answer", in, got.ISOFormat())
		}
	}
}

// fixtureLines splits a behaviour-table fixture the way the oracle adapter does:
// Python's splitlines, empty lines skipped.
func fixtureLines(tb testing.TB, rel string) []string {
	tb.Helper()
	out := []string{}
	for _, line := range pytext.SplitLines(fixture.Text(tb, rel)) {
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func FuzzParseISO(f *testing.F) {
	// Both halves of the split fixture. The widened forms are the higher-entropy
	// seeds of the two -- fractional digits, week dates, basic format -- so
	// dropping them would cost the fuzzer most of what it had to mutate.
	for _, rel := range []string{"docker/started_at.txt", "docker/started_at_py311.txt"} {
		for _, line := range fixtureLines(f, rel) {
			f.Add(line)
		}
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := ParseISO(s)
		if !ok {
			if got != (pytime.Time{}) {
				t.Fatalf("ParseISO(%q) reported no answer but returned %+v", s, got)
			}
			return
		}
		// Nothing that starts with docker's zero value may come back as a time, and
		// nothing may come back naive-with-an-offset. Both are the shapes a future
		// edit to the two guards would take, and both would change what the
		// restart-loop check says rather than merely being wrong.
		if strings.HasPrefix(s, zeroTimePrefix) {
			t.Fatalf("ParseISO(%q) = %s, but docker's zero value is never started", s, got.ISOFormat())
		}
		if !got.Aware && got.Offset != 0 {
			t.Fatalf("ParseISO(%q) = %+v: a naive reading carries an offset", s, got)
		}
	})
}
