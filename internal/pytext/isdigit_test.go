package pytext

import (
	"errors"
	"testing"
	"unicode"
)

func TestIsDigitString(t *testing.T) {
	t.Parallel()

	// Measured against CPython 3.14's str.isdigit().
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"one ascii digit", "0", true},
		{"nine", "9", true},
		{"several ascii digits", "42", true},
		{"arabic-indic digits", "٤٢", true},
		{"thai digit", "๓", true},
		{"osmanya digit", "\U000104a0", true},
		{"mathematical double-struck digit", "\U0001d7dd", true},
		{"mixed scripts still all digits", "1٤2", true},
		{"superscript two", "²", true},
		{"two superscripts", "²³", true},
		{"circled two", "②", true},
		{"circled zero", "⓪", true},
		{"digit zero full stop", "\U0001f100", true},
		// Numeric, but not a digit: a vulgar fraction has a Numeric_Type of
		// Numeric, and a roman numeral is Nl.
		{"one half", "½", false},
		{"one third", "⅓", false},
		{"roman numeral eight", "Ⅷ", false},
		// Circled twenty is not a single digit, so isdigit() is false where
		// isnumeric() would be true. The range stops at circled nine.
		{"circled twenty", "⑳", false},
		{"han numeral", "万", false},
		{"han one", "一", false},
		{"a space between digits", "4 2", false},
		{"sign", "-1", false},
		{"plus sign", "+1", false},
		{"underscore separator", "1_0", false},
		{"decimal point", "1.0", false},
		{"letter", "a", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := IsDigitString(tc.in); got != tc.want {
				t.Errorf("IsDigitString(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestIsDigitStringRejectsInvalidUTF8(t *testing.T) {
	t.Parallel()

	// Python decodes before it classifies, so it never sees a lone byte. Ranging
	// a Go string yields U+FFFD for one, which is not a digit -- the same answer
	// for a different reason, and worth pinning so a later switch to a byte-wise
	// scan cannot quietly accept it.
	if IsDigitString("4\xff2") {
		t.Error("IsDigitString accepted a string containing an invalid UTF-8 byte")
	}
}

func TestNonDecimalDigitTableIsDisjointFromNd(t *testing.T) {
	t.Parallel()

	// The table exists to *add* to unicode.IsDigit. An overlap would mean the
	// enumeration picked up a decimal digit, which is the shape a botched
	// regeneration takes.
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(nonDecimalDigit, r) && unicode.IsDigit(r) {
			t.Errorf("U+%04X is in both nonDecimalDigit and Nd", r)
		}
	}
}

func TestNonDecimalDigitTableHoldsTheMeasuredCount(t *testing.T) {
	t.Parallel()

	// 128 runes in 20 ranges, enumerated from CPython 3.14. A Unicode update that
	// moves one of these into Nd, or adds a new Numeric_Type=Digit rune, should
	// land here as a failure and be re-measured rather than diverge in silence.
	const want = 128

	got := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(nonDecimalDigit, r) {
			got++
		}
	}
	if got != want {
		t.Errorf("nonDecimalDigit holds %d runes, want %d", got, want)
	}
}

func FuzzIsDigitString(f *testing.F) {
	for _, s := range []string{"", "0", "42", "²", "٤٢", "-1", "4 2", "\U0001f100"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := IsDigitString(s)
		if got && s == "" {
			t.Fatal("IsDigitString reported the empty string as a digit string")
		}

		// Every decimal digit string is a digit string, and Int accepts all of
		// them. The non-decimal runes are exactly where those two part company,
		// which is the whole reason this function exists -- so the invariant is
		// asserted over Nd only.
		allNd := s != ""
		for _, r := range s {
			if !unicode.IsDigit(r) {
				allNd = false
				break
			}
		}
		if !allNd {
			return
		}
		if !got {
			t.Fatalf("IsDigitString(%q) = false, but every rune is Nd", s)
		}
		if _, err := Int(s); err != nil && !errors.Is(err, ErrRange) {
			t.Fatalf("Int(%q) = %v, want a value for a decimal digit string", s, err)
		}
	})
}
