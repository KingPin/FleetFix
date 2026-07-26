package pytext

import (
	"strconv"
	"strings"
	"testing"
	"unicode"
)

// The first code point of every Unicode Nd block, generated from CPython:
//
//	python3 -c 'import unicodedata; print(" ".join("%04X"%c for c in range(0x110000)
//	  if unicodedata.category(chr(c))=="Nd" and unicodedata.decimal(chr(c))==0))'
//
// unicodedata 16.0.0, 76 blocks, each verified there as ten ascending code points
// whose decimal values are 0..9. This is the table digitValue deliberately does
// not carry -- it derives the value from the range instead, and this is what says
// the derivation is right.
const cpythonNdBlockZeros = `
0030 0660 06F0 07C0 0966 09E6 0A66 0AE6 0B66 0BE6 0C66 0CE6 0D66 0DE6 0E50 0ED0
0F20 1040 1090 17E0 1810 1946 19D0 1A80 1A90 1B50 1BB0 1C40 1C50 A620 A8D0 A900
A9D0 A9F0 AA50 ABF0 FF10 104A0 10D30 10D40 11066 110F0 11136 111D0 112F0 11450
114D0 11650 116C0 116D0 116DA 11730 118E0 11950 11BF0 11C50 11D50 11DA0 11F50
16130 16A60 16AC0 16B50 16D70 1CCF0 1D7CE 1D7D8 1D7E2 1D7EC 1D7F6 1E140 1E2F0
1E4F0 1E5F1 1E950 1FBF0
`

func cpythonBlocks(t *testing.T) []rune {
	t.Helper()
	var out []rune
	for _, f := range strings.Fields(cpythonNdBlockZeros) {
		n, err := strconv.ParseUint(f, 16, 32)
		if err != nil {
			t.Fatalf("bad block literal %q: %v", f, err)
		}
		out = append(out, rune(n))
	}
	return out
}

// The eight blocks Unicode 16.0 added. Go's unicode tables are 15.0.0 and do not
// have them, so int("𑯰") is 0 to Python and a ValueError here -- a real
// divergence, recorded rather than smoothed over.
//
// Listed as an exact set, and asserted to still be missing: when Go's tables
// catch up, this test fails and the entries come out along with the note in
// Int's doc comment. A tolerant "skip whatever Go lacks" would let the
// divergence outlive the reason for it.
var unicode16OnlyBlocks = map[rune]bool{
	0x10D40: true, // Garay
	0x116D0: true, // Myanmar (Pao and Eastern Pwo, two adjacent blocks)
	0x116DA: true,
	0x11BF0: true, // Sunuwar
	0x16130: true, // Gurung Khema
	0x16D70: true, // Kirat Rai
	0x1CCF0: true, // Outlined digits
	0x1E5F1: true, // Ol Onal
}

// Go's Nd table merges adjacent blocks, so the derivation modulo ten is doing
// real work at U+1D7CE and U+116C0. Check every digit CPython knows about, not
// the block starts alone.
func TestDigitValueAgreesWithCPythonBlocks(t *testing.T) {
	for _, zero := range cpythonBlocks(t) {
		if unicode16OnlyBlocks[zero] {
			if _, ok := digitValue(zero); ok {
				t.Errorf("U+%04X is a digit to Go now (unicode.Version %s); drop it from unicode16OnlyBlocks and from Int's doc comment",
					zero, unicode.Version)
			}
			continue
		}
		for want := range 10 {
			r := zero + rune(want)
			got, ok := digitValue(r)
			if !ok {
				t.Errorf("digitValue(U+%04X) refused it; CPython reads %d", r, want)
				continue
			}
			if got != want {
				t.Errorf("digitValue(U+%04X) = %d, want %d", r, got, want)
			}
		}
	}
}

// The generic half, which also covers any block Go's Unicode version has and the
// CPython that produced the table above did not: every Nd code point must yield
// a value, and reading a whole block through Int must produce that block's number.
func TestDigitValueCoversAllOfNd(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		got, ok := digitValue(r)
		if want := unicode.IsDigit(r); ok != want {
			t.Fatalf("digitValue(U+%04X) accepted=%t; unicode.IsDigit says %t", r, ok, want)
		}
		if ok && (got < 0 || got > 9) {
			t.Fatalf("digitValue(U+%04X) = %d, which is not a decimal digit", r, got)
		}
	}
}

func TestIntReadsAnEntireNdBlock(t *testing.T) {
	for _, zero := range cpythonBlocks(t) {
		if unicode16OnlyBlocks[zero] {
			continue
		}
		var b strings.Builder
		for d := range 10 {
			b.WriteRune(zero + rune(d))
		}
		// Leading zero and all, exactly as Python reads it.
		got, err := Int(b.String())
		if err != nil {
			t.Errorf("Int(%q) (block U+%04X) = %v; want 123456789", b.String(), zero, err)
			continue
		}
		if got != 123456789 {
			t.Errorf("Int(%q) (block U+%04X) = %d, want 123456789", b.String(), zero, got)
		}
	}
}
