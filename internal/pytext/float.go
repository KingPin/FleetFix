package pytext

import (
	"math"
	"strconv"
	"strings"
	"unicode"
)

// Float parses a floating-point number the way Python's float(str) does.
//
// strconv.ParseFloat is the obvious substitute. It agrees on the common shapes
// and differs at five edges, three of which a parser can reach on real input:
//
//   - Surrounding whitespace, stripped by float() and rejected by ParseFloat.
//     Same set as Int: unicode.IsSpace, which is *not* str.isspace() -- float()
//     rejects "5\x1c" exactly as int() does.
//   - Overflow. float("1e400") is inf and float("1e-400") is 0.0; ParseFloat
//     returns those same values *with* ErrRange. Python has no range error here,
//     so neither does this. A parser that treated ErrRange as "unparseable" would
//     drop a reading Python keeps.
//   - Non-ASCII decimal digits. float("١.٢") is 1.2 and float("1e٥") is 100000.
//     The decimal point and the exponent marker stay ASCII, though: "０．５" and
//     "１ｅ２" are both ValueErrors. Measured.
//   - Hex floats. ParseFloat reads "0x1p-2"; Python's float() does not.
//   - Underscores, permitted between digits only -- and only *within* a digit
//     run, so "1_0.0_1" and "1_0e1_0" parse while "1._5", "1_.5", "1e_5" and
//     "5e+_3" are all ValueErrors. Go's rules are close but not the same.
//
// inf/infinity/nan are accepted with an optional sign and any casing, as in both
// languages. The only departure left is the one Int has: Unicode version skew, so
// the eight Nd blocks added in Unicode 16 are digits to float() and syntax errors
// here until Go's tables catch up. See Int.
func Float(s string) (float64, error) {
	// unicode.IsSpace, not IsSpace: float() and int() strip the same set, and it
	// is not the set split() treats as whitespace.
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

	switch strings.ToLower(body) {
	case "inf", "infinity":
		if neg {
			return math.Inf(-1), nil
		}
		return math.Inf(1), nil
	case "nan":
		return math.NaN(), nil
	}

	ascii, ok := foldFloat(body)
	if !ok {
		return 0, ErrSyntax
	}
	if neg {
		ascii = "-" + ascii
	}
	// ParseFloat has nothing left to reject: foldFloat accepts a strict subset of
	// Go's float syntax, having already folded the digits to ASCII. Its only
	// remaining complaint is ErrRange, and the value it returns alongside that --
	// ±Inf on overflow, 0 on underflow -- is precisely what Python returns
	// without complaining at all.
	f, _ := strconv.ParseFloat(ascii, 64)
	return f, nil
}

// foldFloat validates Python's floatnumber grammar (minus the sign and the
// inf/nan spellings, handled above) and rewrites it with ASCII digits.
//
//	digitpart     ::= digit (["_"] digit)*
//	pointfloat    ::= [digitpart] "." digitpart | digitpart "."
//	exponent      ::= ("e" | "E") ["+" | "-"] digitpart
//	floatnumber   ::= (pointfloat | digitpart) [exponent]
//
// Written out rather than delegated to ParseFloat because the underscore rules
// are where the two languages differ, and they differ by *position*: an
// underscore is legal between two digits and nowhere else, which is not
// something a post-hoc strip can tell you.
func foldFloat(s string) (string, bool) {
	r := []rune(s)
	var b strings.Builder
	b.Grow(len(s))
	i := 0

	// digitpart, returning whether it consumed anything.
	digits := func() bool {
		start := i
		for i < len(r) {
			if r[i] == '_' {
				// Legal only with a digit on each side, so the previous rune must
				// have been one and the next must be one too.
				if i == start || i+1 >= len(r) {
					return i > start
				}
				if _, ok := digitValue(r[i+1]); !ok {
					return i > start
				}
				i++
				continue
			}
			v, ok := digitValue(r[i])
			if !ok {
				break
			}
			b.WriteByte(byte('0' + v))
			i++
		}
		return i > start
	}

	whole := digits()
	if i < len(r) && r[i] == '.' {
		b.WriteByte('.')
		i++
		frac := digits()
		if !whole && !frac {
			// "." and "._5" and ".e3": a decimal point needs a digit on one side.
			return "", false
		}
	} else if !whole {
		return "", false
	}

	if i < len(r) && (r[i] == 'e' || r[i] == 'E') {
		b.WriteByte('e')
		i++
		if i < len(r) && (r[i] == '+' || r[i] == '-') {
			b.WriteByte(byte(r[i]))
			i++
		}
		if !digits() {
			// "1.e", "5e" and "5e+_3": the exponent is not optional once the
			// marker is there.
			return "", false
		}
	}

	if i != len(r) {
		// Trailing anything -- a second point, a stray underscore, a unit.
		return "", false
	}
	return b.String(), true
}
