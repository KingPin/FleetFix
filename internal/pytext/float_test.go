package pytext

import (
	"errors"
	"math"
	"strconv"
	"testing"
)

// Every expectation here was produced by running float() on the same literal.
// The grammar cases in particular are not derived from the language reference --
// they are what CPython actually did.

func TestFloat(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want float64
		err  error
	}{
		{name: "integer", in: "5", want: 5},
		{name: "decimal", in: "1.5", want: 1.5},
		{name: "zero", in: "0", want: 0},
		{name: "surrounding whitespace", in: " 5 ", want: 5},
		{name: "nbsp is stripped", in: " 5 ", want: 5},
		{name: "line separator is stripped", in: " 5", want: 5},
		{name: "trailing newline", in: "5\n", want: 5},
		{name: "plus sign", in: "+5", want: 5},
		{name: "minus sign", in: "-1.25", want: -1.25},
		{name: "leading point", in: ".5", want: 0.5},
		{name: "trailing point", in: "5.", want: 5},
		{name: "exponent", in: "1E5", want: 100000},
		{name: "negative exponent", in: "1e-5", want: 1e-5},
		{name: "signed exponent", in: "5E+3", want: 5000},
		{name: "point then exponent", in: "5.e3", want: 5000},
		{name: "whitespace around an exponent form", in: "  .5e1  ", want: 5},

		// Underscores, legal between digits and nowhere else.
		{name: "underscore in the whole part", in: "5_000.5", want: 5000.5},
		{name: "underscores on both sides of the point", in: "1_0.0_1", want: 10.01},
		{name: "underscore in the exponent", in: "1_0e1_0", want: 1e11},
		{name: "underscores everywhere legal", in: "0_0.0_0e0_0", want: 0},
		{name: "underscore before a trailing point", in: "1_0.", want: 10},

		// Nd digits, but ASCII structure.
		{name: "arabic-indic digits", in: "١٢٣", want: 123},
		{name: "arabic-indic with a point", in: "١.٢", want: 1.2},
		{name: "arabic-indic exponent digits", in: "٣e٢", want: 300},
		{name: "ascii mantissa, arabic-indic exponent", in: "1e٥", want: 100000},
		{name: "scripts mixed within one literal", in: "1٢3", want: 123},
		{name: "underscore between non-ascii digits", in: "١_٢.٣", want: 12.3},
		{name: "fullwidth digits", in: "１２３", want: 123},

		{name: "empty", in: "", err: ErrSyntax},
		{name: "whitespace only", in: " ", err: ErrSyntax},
		{name: "sign only", in: "+", err: ErrSyntax},
		{name: "space after the sign", in: "- 5", err: ErrSyntax},
		{name: "point alone", in: ".", err: ErrSyntax},
		{name: "two points", in: "1.2.3", err: ErrSyntax},
		{name: "exponent with no digits", in: "5e", err: ErrSyntax},
		{name: "point then exponent with no digits", in: "1.e", err: ErrSyntax},
		{name: "exponent with no mantissa", in: "e5", err: ErrSyntax},
		{name: "point before an exponent marker", in: ".e3", err: ErrSyntax},
		{name: "hex is not a python float", in: "0x10", err: ErrSyntax},
		{name: "leading underscore", in: "_5", err: ErrSyntax},
		{name: "trailing underscore", in: "5_", err: ErrSyntax},
		{name: "doubled underscore", in: "1__0", err: ErrSyntax},
		{name: "underscore before the point", in: "1_.5", err: ErrSyntax},
		{name: "underscore after the point", in: "1._5", err: ErrSyntax},
		{name: "underscore starting the fraction", in: "._5", err: ErrSyntax},
		{name: "underscore after the exponent marker", in: "1e_5", err: ErrSyntax},
		{name: "underscore ending the exponent", in: "1e5_", err: ErrSyntax},
		{name: "underscore after the exponent sign", in: "5e+_3", err: ErrSyntax},
		{name: "fullwidth exponent marker", in: "１ｅ２", err: ErrSyntax},
		{name: "fullwidth decimal point", in: "０．５", err: ErrSyntax},
		{name: "truncated infinity", in: "infi", err: ErrSyntax},
		{name: "nan with an underscore", in: "nan_", err: ErrSyntax},
		{name: "trailing unit", in: "5ms", err: ErrSyntax},

		// The separators str.isspace() has and float() will not strip -- the same
		// asymmetry Int documents.
		{name: "fs is not stripped", in: "5\x1c", err: ErrSyntax},
		{name: "gs is not stripped", in: "\x1d5", err: ErrSyntax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Float(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("Float(%q) error = %v, want %v", tt.in, err, tt.err)
			}
			if tt.err == nil && got != tt.want {
				t.Errorf("Float(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestFloatInfinityAndNaN(t *testing.T) {
	for _, in := range []string{"inf", "Infinity", "INF", " inf ", " inf "} {
		got, err := Float(in)
		if err != nil || !math.IsInf(got, 1) {
			t.Errorf("Float(%q) = (%v, %v), want +Inf", in, got, err)
		}
	}
	for _, in := range []string{"-inf", "-iNf", "-Infinity"} {
		got, err := Float(in)
		if err != nil || !math.IsInf(got, -1) {
			t.Errorf("Float(%q) = (%v, %v), want -Inf", in, got, err)
		}
	}
	for _, in := range []string{"nan", "+NAN", "-nan"} {
		got, err := Float(in)
		if err != nil || !math.IsNaN(got) {
			t.Errorf("Float(%q) = (%v, %v), want NaN", in, got, err)
		}
	}
}

// Python has no range error: overflow saturates to an infinity and underflow to
// zero, both silently. Go reports ErrRange for the same inputs, so a port that
// forwarded the error would drop readings Python keeps.
func TestFloatOverflowSaturatesInsteadOfFailing(t *testing.T) {
	huge := "1e400"
	if _, err := strconv.ParseFloat(huge, 64); err == nil {
		t.Fatalf("premise changed: strconv.ParseFloat(%q) no longer reports a range error", huge)
	}
	got, err := Float(huge)
	if err != nil || !math.IsInf(got, 1) {
		t.Errorf("Float(%q) = (%v, %v), want +Inf and no error", huge, got, err)
	}

	// A long digit run rather than an exponent, which is the shape a runaway
	// counter in real command output would take.
	long := ""
	for range 400 {
		long += "9"
	}
	if got, err := Float("-" + long); err != nil || !math.IsInf(got, -1) {
		t.Errorf("Float(400 nines, negated) = (%v, %v), want -Inf and no error", got, err)
	}

	if got, err := Float("1e-400"); err != nil || got != 0 {
		t.Errorf("Float(\"1e-400\") = (%v, %v), want 0 and no error", got, err)
	}
}

// The two spellings ParseFloat and float() disagree about on input a parser can
// actually see, pinned so swapping Float out for ParseFloat fails here.
func TestFloatDiffersFromParseFloat(t *testing.T) {
	for _, in := range []string{" 1.5 ", "١.٢"} {
		if _, err := strconv.ParseFloat(in, 64); err == nil {
			t.Fatalf("premise changed: strconv.ParseFloat(%q) now succeeds", in)
		}
		if _, err := Float(in); err != nil {
			t.Errorf("Float(%q) = %v, want it to parse", in, err)
		}
	}
	// And the reverse: Go reads hex floats, Python does not.
	if _, err := strconv.ParseFloat("0x1p-2", 64); err != nil {
		t.Fatalf("premise changed: strconv.ParseFloat(\"0x1p-2\") now fails")
	}
	if _, err := Float("0x1p-2"); err == nil {
		t.Error("Float(\"0x1p-2\") parsed; Python's float() raises")
	}
}

// Anything Float accepts must survive its own rendering, and the only error it
// may report is a syntax error -- range is Python's silent case.
func FuzzFloat(f *testing.F) {
	for _, s := range []string{"1.5", " -2e3 ", "1_0.0_1", ".5", "inf", "nan", "١.٢", "x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Float(s)
		if err != nil {
			if !errors.Is(err, ErrSyntax) {
				t.Fatalf("Float(%q) returned %v; only ErrSyntax is possible", s, err)
			}
			return
		}
		if math.IsNaN(got) {
			return // NaN never round-trips by equality.
		}
		again, err := Float(strconv.FormatFloat(got, 'g', -1, 64))
		if err != nil || again != got {
			t.Fatalf("Float(%q) = %v, but re-parsing its own rendering gave (%v, %v)", s, got, again, err)
		}
	})
}
