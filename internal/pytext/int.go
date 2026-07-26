package pytext

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// ErrSyntax is returned for input Python's int() would reject with a ValueError.
var ErrSyntax = errors.New("pytext: invalid literal for int()")

// ErrRange is returned for a well-formed literal that does not fit in an int64.
//
// Separate from ErrSyntax because it is the one case where this function and
// Python disagree about whether the input is valid at all, and a caller that wants
// to treat the two differently should be able to.
var ErrRange = errors.New("pytext: integer literal out of int64 range")

// Int parses a base-10 integer the way Python's int(str) does.
//
// strconv.ParseInt is the obvious substitute and differs in three ways, two of
// which reach a parser through ordinary-looking input:
//
//   - Surrounding whitespace. int(" 5 ") is 5; ParseInt returns an error. The
//     stripped set is exactly unicode.IsSpace -- measured, and notably *not*
//     str.isspace(), which additionally contains U+001C..U+001F. int("5\x1c")
//     really does raise, so those four are separators to split() and syntax
//     errors to int(), and the two functions here differ accordingly.
//   - Underscore digit separators. int("5_000") is 5000, permitted between digits
//     only -- never leading, trailing, or doubled. ParseInt accepts them only in
//     base 0, where it would also start reading "0x10" as hex, which Python's
//     base-10 int() rejects.
//   - A leading "+" is accepted by both.
//   - Non-ASCII decimal digits. Python accepts every Unicode Nd code point and
//     mixes scripts freely, so int("٣3") is 33 and int("١_٢") is 12, while
//     int("³") and int("½") are ValueErrors -- No and Nl are not Nd. Measured,
//     including the mixing.
//
// Two departures remain, to be recorded in known_divergences.yaml if the harness
// ever reaches them rather than papered over:
//
//   - Magnitude. Python integers are arbitrary precision; these results have to
//     land in an int64 field. A literal above 2^63-1 returns ErrRange where Python
//     returns a value, so a row Python keeps is one this drops. No block count,
//     inode count or byte total from a real tool comes close.
//   - Unicode version. Go's tables are 15.0.0 and CPython 3.14's are 16.0.0, so
//     the eight Nd blocks Unicode 16 added -- Garay, Sunuwar, Kirat Rai, Ol Onal,
//     two Myanmar, Gurung Khema, and the outlined digits -- are digits to int()
//     and syntax errors here. This one expires on its own; a test names the eight
//     and fails when Go catches up.
func Int(s string) (int64, error) {
	// TrimFunc with unicode.IsSpace rather than pytext.IsSpace: this is the one
	// place the two sets must not be the same.
	t := strings.TrimFunc(s, unicode.IsSpace)
	if t == "" {
		return 0, ErrSyntax
	}

	body, neg := t, false
	switch body[0] {
	case '+':
		body = body[1:]
	case '-':
		body, neg = body[1:], true
	}
	if body == "" {
		return 0, ErrSyntax
	}

	// One pass that both validates the underscores and folds every digit down to
	// ASCII, so ParseInt below sees a plain [0-9]+ whatever script the input was
	// written in. Underscore legality is positional -- "1_0" is 10, while "_10",
	// "10_" and "1__0" are all ValueErrors -- which is why it is tracked here
	// rather than by stripping them first and checking the result.
	var b strings.Builder
	b.Grow(len(body))
	prevUnderscore, seenDigit := false, false
	for _, r := range body {
		if r == '_' {
			if !seenDigit || prevUnderscore {
				return 0, ErrSyntax
			}
			prevUnderscore = true
			continue
		}
		v, ok := digitValue(r)
		if !ok {
			return 0, ErrSyntax
		}
		prevUnderscore, seenDigit = false, true
		b.WriteByte(byte('0' + v))
	}
	if prevUnderscore {
		return 0, ErrSyntax
	}

	digits := b.String()
	if neg {
		digits = "-" + digits
	}
	n, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		// digits is already known to match [-]?[0-9]+, so magnitude is the only
		// failure ParseInt has left to report.
		return 0, ErrRange
	}
	return n, nil
}

// digitValue returns the decimal value of r, for every code point Python's int()
// treats as a digit: category Nd, not the wider "looks numeric" set that also
// holds ³ (No) and Ⅻ (Nl).
//
// The value is derived from the position within the Nd range rather than from a
// table of 760 code points. Unicode lays every Nd block out as ten ascending
// code points starting at that script's zero, so the offset within the block is
// the value -- taken modulo ten because Go merges blocks that happen to be
// adjacent, as it does for the five mathematical alphanumeric sets at U+1D7CE.
// A test walks all of Nd and checks the answer against the block table generated
// from CPython's unicodedata, so a merge this reasoning did not anticipate fails
// loudly rather than returning a plausible wrong digit.
func digitValue(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	lo, ok := ndBlockStart(r)
	if !ok {
		return 0, false
	}
	return int((r - lo) % 10), true
}

// ndBlockStart returns the first code point of the unicode.Nd range holding r.
func ndBlockStart(r rune) (rune, bool) {
	if r <= 0xFFFF {
		rs := unicode.Nd.R16
		i := sort.Search(len(rs), func(i int) bool { return rune(rs[i].Hi) >= r })
		if i < len(rs) && rune(rs[i].Lo) <= r && rs[i].Stride == 1 {
			return rune(rs[i].Lo), true
		}
		return 0, false
	}
	rs := unicode.Nd.R32
	i := sort.Search(len(rs), func(i int) bool { return rune(rs[i].Hi) >= r })
	if i < len(rs) && rune(rs[i].Lo) <= r && rs[i].Stride == 1 {
		return rune(rs[i].Lo), true
	}
	return 0, false
}
