package pytext

import (
	"strings"
	"unicode"
)

// The runes str.lower() does not map one rune to one rune.
const (
	// dottedCapitalI is the only rune in Unicode with an unconditional multi-rune
	// lowercase mapping. Every code point from 0 to 0x10FFFF was lowercased in
	// CPython to establish that, rather than taken from SpecialCasing.txt.
	dottedCapitalI    = 'İ'
	combiningDotAbove = '\u0307'

	// capitalSigma lowercases to one of two runes depending on its neighbours.
	capitalSigma = 'Σ'
	smallSigma   = 'σ'
	finalSigma   = 'ς'
)

// Lower returns s lowercased the way Python's str.lower() does.
//
// Not strings.ToLower, which maps each rune independently through the simple
// lowercase mapping. Python applies the full mapping, and it differs in exactly
// two ways -- both measured against CPython 3.14.6 rather than read off a
// property name:
//
//   - U+0130 (LATIN CAPITAL LETTER I WITH DOT ABOVE) lowercases to two runes,
//     "i" followed by U+0307. strings.ToLower gives a bare "i", losing the dot.
//   - U+03A3 (GREEK CAPITAL LETTER SIGMA) lowercases to U+03C2 (final sigma) at
//     the end of a word and U+03C3 elsewhere. strings.ToLower always gives
//     U+03C3, so the last rune of "ΟΔΟΣ" comes out as U+03C3 where Python gives
//     U+03C2.
//
// Nothing else differs, which is what makes the guard cheap: a string holding
// neither rune lowercases one rune at a time, and that is precisely what
// strings.ToLower does.
//
// The port needs this rather than tolerating the difference because v1 compares
// the result of .lower()/.upper() against a fixed string in several places --
// parse_sha256_line matches a digest, tcp.py looks a scheme up in a dict -- so a
// rune that lowercases differently is a lookup that misses, not a cosmetic
// difference in something displayed.
//
// One skew remains, the same one Int and IsDigitString carry: Go's tables are
// Unicode 15 and CPython 3.14's are Unicode 16, so the 27 uppercase letters
// Unicode 16 added lowercase to themselves here and to their real lowercase in
// Python. That is a Go version gap rather than a translation choice, it resolves
// itself when Go's tables update, and shipping a private copy of Unicode 16's
// case data to close it would cost far more than the gap does -- none of the 27
// is reachable from any output a Linux CLI tool produces.
func Lower(s string) string {
	if !strings.ContainsAny(s, string(dottedCapitalI)+string(capitalSigma)) {
		return strings.ToLower(s)
	}

	rs := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	for i, r := range rs {
		switch r {
		case dottedCapitalI:
			b.WriteRune('i')
			b.WriteRune(combiningDotAbove)
		case capitalSigma:
			b.WriteRune(sigmaAt(rs, i))
		default:
			b.WriteRune(unicode.ToLower(r))
		}
	}
	return b.String()
}

// sigmaAt decides which sigma the capital at rs[i] lowercases to.
//
// This is CPython's handle_capital_sigma, transcribed: scan back over
// case-ignorable runes and require a cased one, then scan forward over
// case-ignorable runes and require the run to end without one. Both scans and
// both predicates matter -- "ΑΣ" is final ("ας") while "ΑΣΑ" is not ("ασα"), and
// "ΑΣ’" is final while "ΑΣ’Α" is not, because the apostrophe is skipped.
func sigmaAt(rs []rune, i int) rune {
	j := i - 1
	for ; j >= 0; j-- {
		if !caseIgnorable(rs[j]) {
			break
		}
	}
	if j < 0 || !cased(rs[j]) {
		return smallSigma
	}
	for j = i + 1; j < len(rs); j++ {
		if !caseIgnorable(rs[j]) {
			break
		}
	}
	if j < len(rs) && cased(rs[j]) {
		return smallSigma
	}
	return finalSigma
}

// caseIgnorable reports whether r is skipped by the scans in sigmaAt.
//
// Unicode's Case_Ignorable is the union of five general categories and three
// Word_Break values. Go ships the categories and no Word_Break tables at all, so
// the eight-value part is written out below; it is the one piece of this Go is
// structurally missing rather than merely behind on.
func caseIgnorable(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) ||
		unicode.Is(wordBreakMidLetter, r)
}

// cased reports whether r counts as a cased rune to the scans in sigmaAt.
//
// Only ever asked about a rune that stopped a scan, so it does not have to
// subtract Case_Ignorable: the Lm letters that are both cased and ignorable
// (U+1D43 and friends) never reach it. Go's categories cover this set exactly --
// every code point from 0 to 0x10FFFF was checked against CPython's own answer,
// and the only disagreements were the 52 letters Unicode 16 added.
func cased(r rune) bool {
	return unicode.In(r, unicode.Ll, unicode.Lu, unicode.Lt,
		unicode.Other_Lowercase, unicode.Other_Uppercase)
}

// wordBreakMidLetter holds the Word_Break=MidLetter, MidNumLet and Single_Quote
// runes, which Case_Ignorable includes and Go's unicode package does not expose.
//
// Enumerated by measurement, not from UAX #29: every code point was probed for
// whether CPython skipped it when deciding a neighbouring sigma, and these are
// the ones it skipped that no Go category claims. The count is asserted in the
// test, so a Go release that starts supplying the property -- or a Unicode
// revision that moves a rune in or out of it -- is a failure rather than a
// silent drift.
var wordBreakMidLetter = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x0027, Hi: 0x0027, Stride: 1},
		{Lo: 0x002E, Hi: 0x002E, Stride: 1},
		{Lo: 0x003A, Hi: 0x003A, Stride: 1},
		{Lo: 0x00B7, Hi: 0x00B7, Stride: 1},
		{Lo: 0x0387, Hi: 0x0387, Stride: 1},
		{Lo: 0x055F, Hi: 0x055F, Stride: 1},
		{Lo: 0x05F4, Hi: 0x05F4, Stride: 1},
		{Lo: 0x2018, Hi: 0x2019, Stride: 1},
		{Lo: 0x2024, Hi: 0x2024, Stride: 1},
		{Lo: 0x2027, Hi: 0x2027, Stride: 1},
		{Lo: 0xFE13, Hi: 0xFE13, Stride: 1},
		{Lo: 0xFE52, Hi: 0xFE52, Stride: 1},
		{Lo: 0xFE55, Hi: 0xFE55, Stride: 1},
		{Lo: 0xFF07, Hi: 0xFF07, Stride: 1},
		{Lo: 0xFF0E, Hi: 0xFF0E, Stride: 1},
		{Lo: 0xFF1A, Hi: 0xFF1A, Stride: 1},
	},
	LatinOffset: 4,
}
