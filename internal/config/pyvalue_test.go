package config

import (
	"math"
	"math/big"
	"testing"
	"time"
)

// Every case here was checked against python3 -c 'print(bool(x))'. The interesting
// ones are the values that read as false and are not: a quoted "false", and NaN.
func TestPyTruthy(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, false},
		{"true", true, true},
		{"false", false, false},
		{"zero int", int64(0), false},
		{"int", int64(1), true},
		{"negative int", int64(-1), true},
		{"zero big int", big.NewInt(0), false},
		{"big int", new(big.Int).SetUint64(math.MaxUint64), true},
		{"zero float", 0.0, false},
		{"negative zero float", math.Copysign(0, -1), false},
		{"float", 0.1, true},
		// bool(float("nan")) is True: it is not equal to zero.
		{"nan", math.NaN(), true},
		{"infinity", math.Inf(1), true},
		{"empty string", "", false},
		// The wart worth having a test for: a quoted "false" in YAML is a non-empty
		// string, and bool() of it is True.
		{"the string false", "false", true},
		{"the string zero", "0", true},
		{"empty binary", []byte{}, false},
		{"binary", []byte("x"), true},
		// A datetime is always truthy, even at the epoch.
		{"zero time", time.Time{}, true},
		{"empty list", []any{}, false},
		{"list", []any{nil}, true},
		{"empty map", map[string]any{}, false},
		{"map", map[string]any{"a": nil}, true},
		{"empty set", map[string]struct{}{}, false},
		{"set", map[string]struct{}{"a": {}}, true},
		// Nothing else is in the vocabulary, and an object with neither __bool__ nor
		// __len__ is true.
		{"outside the vocabulary", struct{}{}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PyTruthy(tc.v); got != tc.want {
				t.Errorf("PyTruthy(%#v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

func TestPyStr(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"nil", nil, "None"},
		{"true", true, "True"},
		{"false", false, "False"},
		{"int", int64(12345), "12345"},
		{"negative int", int64(-1), "-1"},
		{"big int", new(big.Int).SetUint64(math.MaxUint64), "18446744073709551615"},
		// repr(1.5) is "1.5" and repr(1.0) is "1.0" -- the trailing .0 is not
		// cosmetic, it is how Python tells a float from an int.
		{"float", 1.5, "1.5"},
		{"integral float", 1.0, "1.0"},
		{"small float", 0.00001, "1e-05"},
		{"large float", 1e16, "1e+16"},
		// str() and json.dumps part company here: "inf", not "Infinity".
		{"infinity", math.Inf(1), "inf"},
		{"negative infinity", math.Inf(-1), "-inf"},
		{"nan", math.NaN(), "nan"},
		{"string", "already", "already"},
		{"empty string", "", ""},
		// str(datetime) is isoformat with a space for the T.
		{"time", time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC), "2024-01-02 03:04:05+00:00"},
		{
			"time with microseconds",
			time.Date(2024, 1, 2, 3, 4, 5, 500000000, time.UTC),
			"2024-01-02 03:04:05.500000+00:00",
		},
		{
			"time with an offset",
			time.Date(2024, 1, 2, 3, 4, 5, 0, time.FixedZone("", -5*3600-1800)),
			"2024-01-02 03:04:05-05:30",
		},
		// str() of a container is repr() of a container, which is why these are
		// Python's spelling and not Go's. Measured: str([1]) is "[1]" either way, but
		// str(b"hi") is "b'hi'" and not "[104 105]".
		{"binary", []byte("hi"), "b'hi'"},
		{"list", []any{int64(1)}, "[1]"},
		{"map", map[string]any{"a": int64(1)}, "{'a': 1}"},
		{"set", map[string]struct{}{"a": {}}, "{'a'}"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PyStr(tc.v); got != tc.want {
				t.Errorf("PyStr(%#v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// Every want here is the output of `python3 -c "print(repr(x))"` on CPython 3.14.6,
// pasted rather than derived. The scalars are only interesting where repr and str
// disagree, which for this vocabulary is exactly str and bytes.
func TestPyRepr(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"nil", nil, "None"},
		{"true", true, "True"},
		{"int", int64(42), "42"},
		{"big int", bigFromString(t, "12345678901234567890123456789"), "12345678901234567890123456789"},
		{"float", 1.5, "1.5"},
		{"nan", math.NaN(), "nan"},
		{"plain string", "8.8.8.8", "'8.8.8.8'"},
		{"empty string", "", "''"},
		// The quote rule, both ways round. A lone apostrophe switches the quoting;
		// once a double quote is present too, single wins and the apostrophe escapes.
		{"apostrophe", "it's", `"it's"`},
		{"both quotes", `it's a "quote"`, `'it\'s a "quote"'`},
		{"double quote only", `say "hi"`, `'say "hi"'`},
		{"backslash", `a\b`, `'a\\b'`},
		{"newline", "a\nb", `'a\nb'`},
		{"tab and cr", "a\tb\rc", `'a\tb\rc'`},
		// The separators str.strip() removes and int() does not, which is why they
		// turn up in fixtures at all.
		{"unit separators", "\x1c8.8.8.8\x1f", `'\x1c8.8.8.8\x1f'`},
		// Printability, not ASCII-ness, decides: an accented letter and an emoji are
		// printable and stay literal, while NBSP and a zero-width space do not.
		{"nbsp", "a\u00a0b", `'a\xa0b'`},
		{"accent", "café", "'café'"},
		{"zero width space", "a\u200bb", `'a\u200bb'`},
		{"astral", "a\U0001f600b", "'a\U0001f600b'"},
		{"empty list", []any{}, "[]"},
		{"list", []any{"8.8.8.8", "1.1.1.1"}, "['8.8.8.8', '1.1.1.1']"},
		{"nested list", []any{int64(1), []any{2.5, nil}, true}, "[1, [2.5, None], True]"},
		{"empty map", map[string]any{}, "{}"},
		{"map", map[string]any{"b": int64(2), "a": "x"}, "{'a': 'x', 'b': 2}"},
		// An empty set has no literal in Python, so repr calls the constructor.
		{"empty set", map[string]struct{}{}, "set()"},
		{"set", map[string]struct{}{"a": {}}, "{'a'}"},
		{"bytes", []byte("hi"), "b'hi'"},
		{"empty bytes", []byte{}, "b''"},
		// Byte-wise: 0xff is not valid UTF-8, and decoding it as a rune first would
		// have produced �.
		{"high bytes", []byte{0xff, 0x00, '\n', '\''}, `b"\xff\x00\n'"`},
		{"bytes with an apostrophe", []byte("it's"), `b"it's"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PyRepr(tc.v); got != tc.want {
				t.Errorf("PyRepr(%#v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}

// The documented limit, pinned so that changing it is a decision. Python reprs a
// timestamp as the constructor call and would tell a date from a datetime; time.Time
// keeps neither, so it strs.
func TestPyReprSpellsATimestampAsStrDoes(t *testing.T) {
	ts := time.Date(2026, 7, 26, 14, 5, 6, 0, time.UTC)
	const want = "2026-07-26 14:05:06+00:00" // Python: datetime.datetime(2026, 7, 26, 14, 5, 6, tzinfo=...)
	if got := PyRepr(ts); got != want {
		t.Errorf("PyRepr(%v) = %q, want %q", ts, got, want)
	}
}

func bigFromString(tb testing.TB, s string) *big.Int {
	tb.Helper()
	n, ok := new(big.Int).SetString(s, 10)
	if !ok {
		tb.Fatalf("SetString(%q) failed", s)
	}
	return n
}
