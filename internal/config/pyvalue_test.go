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
		// The documented limit: Python spells these with repr and this does not. They
		// are here to pin what the fallback actually produces, so a change to it is a
		// visible decision rather than a surprise in someone's OTLP headers.
		{"binary", []byte("hi"), "[104 105]"},
		{"list", []any{int64(1)}, "[1]"},
		{"map", map[string]any{"a": int64(1)}, "map[a:1]"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := PyStr(tc.v); got != tc.want {
				t.Errorf("PyStr(%#v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
