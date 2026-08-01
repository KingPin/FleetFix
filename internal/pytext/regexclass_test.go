package pytext

import (
	"regexp"
	"testing"
	"unicode"
)

// Every code point, not a sample. The classes exist because the difference
// between Go's \s and Python's is four control characters and fourteen Unicode
// spaces -- a sampled test would miss all eighteen.

func TestSpaceMatchesIsSpace(t *testing.T) {
	space := regexp.MustCompile(`^` + Space + `$`)
	notSpace := regexp.MustCompile(`^` + NotSpace + `$`)
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Cs, r) {
			// Surrogates are not encodable as UTF-8, so a Go string cannot hold
			// one to match against. Python's re would see them; nothing that
			// reaches a parser here can.
			continue
		}
		s := string(r)
		if got, want := space.MatchString(s), IsSpace(r); got != want {
			t.Fatalf("pytext.Space matched %t for U+%04X; IsSpace says %t", got, r, want)
		}
		if got, want := notSpace.MatchString(s), !IsSpace(r); got != want {
			t.Fatalf("pytext.NotSpace matched %t for U+%04X; IsSpace says %t", got, r, !want)
		}
	}
}

func TestDigitMatchesNd(t *testing.T) {
	digit := regexp.MustCompile(`^` + Digit + `$`)
	notDigit := regexp.MustCompile(`^` + NotDigit + `$`)
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.Is(unicode.Cs, r) {
			continue
		}
		s := string(r)
		if got, want := digit.MatchString(s), unicode.IsDigit(r); got != want {
			t.Fatalf("pytext.Digit matched %t for U+%04X; unicode.IsDigit says %t", got, r, want)
		}
		if got, want := notDigit.MatchString(s), !unicode.IsDigit(r); got != want {
			t.Fatalf("pytext.NotDigit matched %t for U+%04X; unicode.IsDigit says %t", got, r, !want)
		}
	}
}

// The four separators are the whole reason IsSpace exists rather than
// unicode.IsSpace, so they get named cases: a regression here is silent
// everywhere else.
func TestSpaceCoversTheControlSeparators(t *testing.T) {
	space := regexp.MustCompile(`^` + Space + `$`)
	for _, r := range []rune{0x1C, 0x1D, 0x1E, 0x1F} {
		if !space.MatchString(string(r)) {
			t.Errorf("pytext.Space does not match U+%04X; Python's \\s does", r)
		}
	}
	// And the ones Go's own \s omits for a different reason -- they are not
	// ASCII.
	for _, r := range []rune{0x0B, 0x85, 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000} {
		if !space.MatchString(string(r)) {
			t.Errorf("pytext.Space does not match U+%04X; Python's \\s does", r)
		}
	}
}

// A digit Go's \d refuses and Python's accepts, all the way through to Int --
// the pairing the doc comment claims.
func TestDigitAndIntAgreeOnNonASCIIDigits(t *testing.T) {
	digits := regexp.MustCompile(Digit + `+`)
	// U+0660..U+0662, ARABIC-INDIC DIGIT ZERO/ONE/TWO.
	const arabic = "٠١٢"
	run := digits.FindString("x" + arabic + "x")
	if run != arabic {
		t.Fatalf("pytext.Digit matched %q; want %q", run, arabic)
	}
	got, err := Int(run)
	if err != nil {
		t.Fatalf("Int(%q) = %v; Python's int() returns 12", run, err)
	}
	if got != 12 {
		t.Errorf("Int(%q) = %d; want 12", run, got)
	}
}
