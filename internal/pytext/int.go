package pytext

import (
	"errors"
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
//
// Two deliberate departures from Python, to be recorded in known_divergences.yaml
// when the harness first reaches them rather than papered over:
//
//   - Non-ASCII decimal digits. Python accepts all 760 Unicode Nd code points, so
//     int("١٢٣") is 123. This accepts ASCII only. Reaching a parser needs a
//     well-formed multi-byte digit sequence in a position a numeric field is read
//     from, which no captured output contains and byte-level fuzzing will
//     essentially never construct.
//   - Magnitude. Python integers are arbitrary precision; these results have to
//     land in an int64 field. A literal above 2^63-1 returns ErrRange where Python
//     returns a value, so a row Python keeps is one this drops. No block count,
//     inode count or byte total from a real tool comes close.
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

	// Underscores are stripped only after being validated in place, because their
	// legality is positional: "1_0" is 10, while "_10", "10_" and "1__0" are all
	// ValueErrors.
	if strings.Contains(body, "_") {
		clean, ok := stripUnderscores(body)
		if !ok {
			return 0, ErrSyntax
		}
		body = clean
	}
	for i := range len(body) {
		if body[i] < '0' || body[i] > '9' {
			return 0, ErrSyntax
		}
	}

	if neg {
		body = "-" + body
	}
	n, err := strconv.ParseInt(body, 10, 64)
	if err != nil {
		// body is already known to match [-]?[0-9]+, so magnitude is the only
		// failure ParseInt has left to report.
		return 0, ErrRange
	}
	return n, nil
}

// stripUnderscores removes separators, reporting false if any sits somewhere
// Python does not allow: at either end, or next to another underscore.
func stripUnderscores(body string) (string, bool) {
	if body[0] == '_' || body[len(body)-1] == '_' {
		return "", false
	}
	var b strings.Builder
	b.Grow(len(body))
	prevUnderscore := false
	for i := range len(body) {
		c := body[i]
		if c == '_' {
			if prevUnderscore {
				return "", false
			}
			prevUnderscore = true
			continue
		}
		prevUnderscore = false
		b.WriteByte(c)
	}
	return b.String(), true
}
