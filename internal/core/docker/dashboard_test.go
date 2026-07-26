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
