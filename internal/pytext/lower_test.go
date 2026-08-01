package pytext

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// Expectations come from CPython 3.14.6's str.lower() fed the same literals.
//
// Every rune the cases are built from is named here rather than written inline,
// and the invisible ones are escapes. The whole subject is the difference between
// U+03C3 and U+03C2, and between "i" and "i" followed by a combining dot --
// differences a proportional font renders as nearly or exactly the same thing,
// which is precisely how a wrong expectation would survive review.
const (
	capSigma   = "Σ" // GREEK CAPITAL LETTER SIGMA
	lcSigma    = "σ" // GREEK SMALL LETTER SIGMA
	lcFinal    = "ς" // GREEK SMALL LETTER FINAL SIGMA
	capAlpha   = "Α" // GREEK CAPITAL LETTER ALPHA
	lcAlpha    = "α" // GREEK SMALL LETTER ALPHA
	capOmicron = "Ο" // GREEK CAPITAL LETTER OMICRON
	lcOmicron  = "ο" // GREEK SMALL LETTER OMICRON
	capDelta   = "Δ" // GREEK CAPITAL LETTER DELTA
	lcDelta    = "δ" // GREEK SMALL LETTER DELTA

	capI     = "İ"      // LATIN CAPITAL LETTER I WITH DOT ABOVE
	dotAbove = "\u0307" // COMBINING DOT ABOVE
	dotlessI = "ı"      // LATIN SMALL LETTER DOTLESS I

	acute      = "\u0301" // COMBINING ACUTE ACCENT: Mn, so case-ignorable
	rightQuote = "’"      // RIGHT SINGLE QUOTATION MARK: Word_Break=MidNumLet
	modifierA  = "ᵃ"      // MODIFIER LETTER SMALL A: Ll and Lm, so both at once

	capDZ     = "Ǆ" // LATIN CAPITAL LETTER DZ WITH CARON
	titleDZ   = "ǅ" // LATIN CAPITAL LETTER D WITH SMALL LETTER Z WITH CARON
	smallDZ   = "ǆ" // LATIN SMALL LETTER DZ WITH CARON
	sharpS    = "ß" // LATIN SMALL LETTER SHARP S
	capSharpS = "ẞ" // LATIN CAPITAL LETTER SHARP S
	kelvin    = "K" // KELVIN SIGN
)

func TestLower(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"already lower", "abc", "abc"},
		{"ascii", "ABC123", "abc123"},
		{"ascii with punctuation", "Hello, World!", "hello, world!"},
		{"latin-1", "ÀÉÎÕÜ", "àéîõü"},

		// Simple mappings Go already agrees about, here so the escapes above are
		// exercised as themselves and not only as sigma's neighbours.
		{"kelvin sign", kelvin, "k"},
		{"sharp s is already lower", sharpS, sharpS},
		{"capital sharp s", capSharpS, sharpS},
		{"uppercase digraph", capDZ, smallDZ},
		{"titlecase digraph", titleDZ, smallDZ},
		{"dotless i is already lower", dotlessI, dotlessI},
		{"plain capital i", "I", "i"},

		// The one unconditional multi-rune mapping: strings.ToLower would drop the
		// dot and give a bare "i".
		{"dotted capital i", capI, "i" + dotAbove},
		{"dotted capital i twice", capI + capI, "i" + dotAbove + "i" + dotAbove},
		{"dotted capital i inside a word", "a" + capI + "b", "a" + "i" + dotAbove + "b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Lower(tt.in); got != tt.want {
				t.Errorf("Lower(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestLowerFinalSigma covers the contextual mapping, which is the part
// strings.ToLower gets wrong on every input that reaches it.
func TestLowerFinalSigma(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Nothing cased before it, so not word-final however it looks.
		{"alone", capSigma, lcSigma},
		{"leading", capSigma + capAlpha, lcSigma + lcAlpha},
		{"after a space", capAlpha + " " + capSigma, lcAlpha + " " + lcSigma},
		{"after a digit", "1" + capSigma, "1" + lcSigma},

		// A cased rune before and none after.
		{"trailing", capAlpha + capSigma, lcAlpha + lcFinal},
		{"before a digit", capAlpha + capSigma + "1", lcAlpha + lcFinal + "1"},
		{"between cased runes", capAlpha + capSigma + capAlpha, lcAlpha + lcSigma + lcAlpha},
		{"two sigmas", capSigma + capSigma, lcSigma + lcFinal},
		{
			"a whole word",
			capOmicron + capDelta + capOmicron + capSigma,
			lcOmicron + lcDelta + lcOmicron + lcFinal,
		},
		{
			"two words",
			capOmicron + capDelta + capOmicron + capSigma + " " + capAlpha + capSigma,
			lcOmicron + lcDelta + lcOmicron + lcFinal + " " + lcAlpha + lcFinal,
		},

		// Combining marks are case-ignorable, so both scans skip them.
		{"accent before", capAlpha + acute + capSigma, lcAlpha + acute + lcFinal},
		{"accent after", capAlpha + capSigma + acute, lcAlpha + lcFinal + acute},
		{"accents both sides", capAlpha + acute + capSigma + acute, lcAlpha + acute + lcFinal + acute},
		{"accent then a cased rune", capAlpha + capSigma + acute + capAlpha, lcAlpha + lcSigma + acute + lcAlpha},

		// A modifier letter is cased and case-ignorable at once; ignorable wins,
		// so it is skipped rather than counted as the cased rune the rule needs.
		{"modifier letter before", modifierA + capSigma, modifierA + lcSigma},
		{"modifier letter is skipped", capAlpha + modifierA + capSigma, lcAlpha + modifierA + lcFinal},

		// The Word_Break values Go's tables do not carry. Without the hand-written
		// table these would all come out as plain U+03C3.
		{"full stop before", capAlpha + "." + capSigma, lcAlpha + "." + lcFinal},
		{"colon before", capAlpha + ":" + capSigma, lcAlpha + ":" + lcFinal},
		{"apostrophe after", capAlpha + capSigma + rightQuote, lcAlpha + lcFinal + rightQuote},
		{
			"apostrophe then a cased rune",
			capAlpha + capSigma + rightQuote + capAlpha,
			lcAlpha + lcSigma + rightQuote + lcAlpha,
		},

		// The dot the dotted capital I expands to is itself case-ignorable, so a
		// sigma after one still sees the I as its cased neighbour.
		{"after a dotted capital i", capI + capSigma, "i" + dotAbove + lcFinal},
		{"around a dotted capital i", capSigma + capI + capSigma, lcSigma + "i" + dotAbove + lcFinal},

		{"uppercase digraph before", capDZ + capSigma, smallDZ + lcFinal},
		{"titlecase digraph before", titleDZ + capSigma, smallDZ + lcFinal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Lower(tt.in); got != tt.want {
				t.Errorf("Lower(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestWordBreakMidLetterIsOnlyWhatGoLacks keeps the hand-written table honest.
//
// Every rune in it must be absent from the five categories caseIgnorable already
// consults, or the entry is dead weight that reads like a measured finding. The
// count is pinned for the reason isdigit.go pins its own: a Go release that starts
// supplying Word_Break, or a Unicode revision that moves a rune into or out of the
// property, should be a failing test and not a silent drift.
func TestWordBreakMidLetterIsOnlyWhatGoLacks(t *testing.T) {
	n := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.Is(wordBreakMidLetter, r) {
			continue
		}
		n++
		if unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) {
			t.Errorf("%U is in the table and already in a category caseIgnorable consults", r)
		}
		if !caseIgnorable(r) {
			t.Errorf("caseIgnorable(%U) = false, want true", r)
		}
	}
	if want := 17; n != want {
		t.Errorf("wordBreakMidLetter holds %d runes, want %d", n, want)
	}
}

// TestModifierLetterIsBothCasedAndIgnorable pins the premise behind the shortcut
// in cased's doc comment.
//
// cased omits the Case_Ignorable subtraction the Unicode definition implies,
// which is sound only because sigmaAt asks it exclusively about a rune that
// stopped one of its scans -- and a rune in both sets never stops a scan. U+1D43
// is the witness: if the two predicates ever stopped overlapping, the shortcut
// would be describing a situation that no longer exists.
func TestModifierLetterIsBothCasedAndIgnorable(t *testing.T) {
	const r = 'ᵃ'
	if !caseIgnorable(r) {
		t.Errorf("caseIgnorable(%U) = false, want true: it is Lm", r)
	}
	if !cased(r) {
		t.Errorf("cased(%U) = false, want true: it is Ll", r)
	}
}

// TestLowerDepartsOnUnicodeVersionSkew records where Go's Unicode 15 tables and
// CPython's Unicode 16 disagree. Neither is a translation choice, and both
// resolve themselves when Go's tables update -- at which point this test fails
// and gets deleted, which is the point of writing it down.
func TestLowerDepartsOnUnicodeVersionSkew(t *testing.T) {
	// U+10D50 is an uppercase Garay letter added in Unicode 16. Python lowercases
	// it to U+10D70; Go does not know it has a mapping.
	if got := Lower("\U00010D50"); got != "\U00010D50" {
		t.Errorf("Lower(U+10D50) = %q, want it unchanged while Go's tables are Unicode 15", got)
	}

	// U+1171E was Mn in Unicode 15 and Mc in Unicode 16, so Go skips it as
	// case-ignorable where Python stops on it as an uncased rune. Note the code
	// point: U+1171 followed by "E" is a different string that proves nothing.
	in := capAlpha + "\U0001171E" + capSigma
	if got, want := Lower(in), lcAlpha+"\U0001171E"+lcFinal; got != want {
		t.Errorf("Lower(%q) = %q, want %q, the final sigma Go's Unicode 15 tables produce", in, got, want)
	}
}

func FuzzLower(f *testing.F) {
	for _, seed := range []string{
		"", "abc", "ABC123", "0123456789abcdef",
		capI, capSigma, capAlpha + capSigma, capAlpha + capSigma + capAlpha,
		capAlpha + acute + capSigma + rightQuote,
		modifierA + capSigma,
		capOmicron + capDelta + capOmicron + capSigma,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		got := Lower(in)

		// Lowercasing is idempotent in CPython -- checked there over every code
		// point and over every combination of the runes that reach the special
		// paths -- so a second pass that changes something means the contextual
		// rule read a context this function itself created.
		if again := Lower(got); again != got {
			t.Fatalf("Lower(%q) = %q, lowering that again gives %q", in, got, again)
		}

		// Valid input stays valid: the sigma path indexes a []rune, so a bug there
		// would surface as a replacement character rather than as a wrong letter.
		if utf8.ValidString(in) && !utf8.ValidString(got) {
			t.Fatalf("Lower(%q) produced invalid UTF-8: %q", in, got)
		}

		// The whole guard rests on this: with neither special rune present, the
		// stdlib's per-rune mapping is what Python does.
		if !strings.ContainsAny(in, capI+capSigma) {
			if want := strings.ToLower(in); got != want {
				t.Fatalf("Lower(%q) = %q, want strings.ToLower's %q", in, got, want)
			}
		}
	})
}
