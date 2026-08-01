package pytext

import "unicode"

// IsDigitString reports whether s is a digit string to Python's str.isdigit():
// non-empty, with every rune a digit.
//
// Not unicode.IsDigit over the runes. str.isdigit() is true for the decimal
// category Nd *and* for 128 further runes carrying a Numeric_Type of Digit --
// superscripts, subscripts, and the circled/parenthesised/full-stop digit forms.
// Those extra runes are exactly the ones int() then rejects, which is why the
// distinction matters here rather than being a curiosity: v1's read_meminfo
// guards with isdigit() and converts with int(), so a value of "²" passes
// the guard and raises ValueError. Reproducing that needs both sets.
//
// The 128 runes were enumerated from CPython rather than derived from a property
// name, and the count is asserted in the test so a Unicode update that grows the
// set is a failure rather than a silent drift. Nd itself comes from Go's tables,
// so the version skew Int documents applies here too: a rune added to Nd in
// Unicode 16 is a digit to Python and not to this until Go catches up.
func IsDigitString(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !unicode.IsDigit(r) && !unicode.Is(nonDecimalDigit, r) {
			return false
		}
	}
	return true
}

// nonDecimalDigit holds the runes str.isdigit() accepts that are not Nd.
var nonDecimalDigit = &unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x00B2, Hi: 0x00B3, Stride: 1},
		{Lo: 0x00B9, Hi: 0x00B9, Stride: 1},
		{Lo: 0x1369, Hi: 0x1371, Stride: 1},
		{Lo: 0x19DA, Hi: 0x19DA, Stride: 1},
		{Lo: 0x2070, Hi: 0x2070, Stride: 1},
		{Lo: 0x2074, Hi: 0x2079, Stride: 1},
		{Lo: 0x2080, Hi: 0x2089, Stride: 1},
		{Lo: 0x2460, Hi: 0x2468, Stride: 1},
		{Lo: 0x2474, Hi: 0x247C, Stride: 1},
		{Lo: 0x2488, Hi: 0x2490, Stride: 1},
		{Lo: 0x24EA, Hi: 0x24EA, Stride: 1},
		{Lo: 0x24F5, Hi: 0x24FD, Stride: 1},
		{Lo: 0x24FF, Hi: 0x24FF, Stride: 1},
		{Lo: 0x2776, Hi: 0x277E, Stride: 1},
		{Lo: 0x2780, Hi: 0x2788, Stride: 1},
		{Lo: 0x278A, Hi: 0x2792, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x10A40, Hi: 0x10A43, Stride: 1},
		{Lo: 0x10E60, Hi: 0x10E68, Stride: 1},
		{Lo: 0x11052, Hi: 0x1105A, Stride: 1},
		{Lo: 0x1F100, Hi: 0x1F10A, Stride: 1},
	},
	LatinOffset: 2,
}
