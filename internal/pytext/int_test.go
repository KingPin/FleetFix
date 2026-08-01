package pytext

import (
	"errors"
	"strconv"
	"testing"
)

func TestInt(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
		err  error
	}{
		{name: "plain", in: "5", want: 5},
		{name: "zero", in: "0", want: 0},
		{name: "surrounding whitespace", in: " 5 ", want: 5},
		{name: "tab and newline are stripped", in: "\t5\n", want: 5},
		{name: "nbsp is stripped", in: "\u00a05\u00a0", want: 5},
		{name: "plus sign", in: "+5", want: 5},
		{name: "minus sign", in: "-5", want: -5},
		{name: "sign after whitespace", in: "  -5", want: -5},
		{name: "leading zeros", in: "007", want: 7},
		{name: "underscore separator", in: "5_000", want: 5000},
		{name: "several separators", in: "1_2_3", want: 123},
		{name: "separator with sign", in: "-1_000", want: -1000},
		{name: "max int64", in: "9223372036854775807", want: 9223372036854775807},
		{name: "min int64", in: "-9223372036854775808", want: -9223372036854775808},

		// The four separators split() treats as whitespace and int() does not.
		// This asymmetry is real CPython behaviour, not an oversight here.
		{name: "fs is not stripped", in: "5\x1c", err: ErrSyntax},
		{name: "gs is not stripped", in: "\x1d5", err: ErrSyntax},

		{name: "empty", in: "", err: ErrSyntax},
		{name: "whitespace only", in: "   ", err: ErrSyntax},
		{name: "sign only", in: "-", err: ErrSyntax},
		{name: "double sign", in: "--5", err: ErrSyntax},
		{name: "leading underscore", in: "_5", err: ErrSyntax},
		{name: "trailing underscore", in: "5_", err: ErrSyntax},
		{name: "doubled underscore", in: "5__0", err: ErrSyntax},
		{name: "underscore after sign", in: "-_5", err: ErrSyntax},
		{name: "hex is not base 10", in: "0x10", err: ErrSyntax},
		{name: "float is not an int", in: "5.0", err: ErrSyntax},
		{name: "df's dash for no accounting", in: "-", err: ErrSyntax},
		{name: "internal space", in: "1 2", err: ErrSyntax},
		{name: "superscript two is not a digit", in: "²", err: ErrSyntax},

		// Documented departure: Python returns a value here.
		{name: "above int64", in: "9223372036854775808", err: ErrRange},
		{name: "below int64", in: "-9223372036854775809", err: ErrRange},

		// Nd, in every spelling Python accepts. Pinned by running int() on each.
		{name: "arabic-indic digits", in: "١٢٣", want: 123},
		{name: "scripts mixed within one literal", in: "٣3", want: 33},
		{name: "underscore between non-ascii digits", in: "١_٢", want: 12},
		{name: "extended arabic-indic", in: "۵", want: 5},
		{name: "mathematical double-struck", in: "𝟝", want: 5},
		{name: "fullwidth", in: "１２３", want: 123},
		{name: "thai", in: "๓", want: 3},
		{name: "signed non-ascii", in: "-٣", want: -3},
		{name: "non-ascii with surrounding space", in: " ٣ ", want: 3},
		// No and Nl look numeric and are not digits to int().
		{name: "vulgar fraction is not a digit", in: "½", err: ErrSyntax},
		{name: "roman numeral is not a digit", in: "Ⅻ", err: ErrSyntax},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Int(tt.in)
			if !errors.Is(err, tt.err) {
				t.Fatalf("Int(%q) error = %v, want %v", tt.in, err, tt.err)
			}
			if tt.err == nil && got != tt.want {
				t.Errorf("Int(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// strconv.ParseInt is what a port reaches for first. Pin the two places it gives a
// different answer on input a parser can actually see, so replacing Int with it
// fails here rather than on a host.
func TestIntDiffersFromParseInt(t *testing.T) {
	for _, in := range []string{" 5 ", "5_000"} {
		if _, err := strconv.ParseInt(in, 10, 64); err == nil {
			t.Fatalf("premise changed: strconv.ParseInt(%q) now succeeds", in)
		}
		if _, err := Int(in); err != nil {
			t.Errorf("Int(%q) = %v, want it to parse", in, err)
		}
	}
	// The reverse: base 0 would accept hex, which base-10 int() rejects.
	if _, err := strconv.ParseInt("0x10", 0, 64); err != nil {
		t.Fatalf("premise changed: ParseInt(\"0x10\", 0) now fails")
	}
	if _, err := Int("0x10"); err == nil {
		t.Error("Int(\"0x10\") parsed; Python's base-10 int() raises")
	}
}

// Anything Int accepts, strconv must agree on once the Python-only spellings are
// removed -- the arithmetic is not being reimplemented, only the syntax.
func FuzzInt(f *testing.F) {
	for _, s := range []string{"5", " -12 ", "1_0", "0", "9223372036854775807", "x"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := Int(s)
		if err != nil {
			if !errors.Is(err, ErrSyntax) && !errors.Is(err, ErrRange) {
				t.Fatalf("Int(%q) returned an unclassified error: %v", s, err)
			}
			return
		}
		// Round-tripping the accepted value must reproduce it exactly.
		if again, err := Int(strconv.FormatInt(got, 10)); err != nil || again != got {
			t.Fatalf("Int(%q) = %d, but re-parsing its own rendering gave (%d, %v)", s, got, again, err)
		}
	})
}
