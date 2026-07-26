package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
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
