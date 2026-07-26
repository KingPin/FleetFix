// Package pytext reproduces the Python string primitives the v1 parsers are built
// on, where Go's equivalents differ.
//
// Nearly every parser in internal/core is a port of a function whose first two
// lines are `for line in text.splitlines()` and `parts = line.split(None, n)`. Go
// has no exact counterpart to either, and the near-misses diverge on inputs a
// differential harness will find:
//
//   - strings.Split(s, "\n") is not str.splitlines(). Python breaks on eleven
//     boundaries, treats CRLF as one, and drops the empty trailing element a final
//     boundary would otherwise produce.
//   - strings.Fields is not str.split(None): it splits on unicode.IsSpace, which
//     omits U+001C..U+001F. Python treats those four as whitespace.
//   - strings.SplitN(s, sep, n) is not str.split(None, maxsplit): it takes a single
//     literal separator rather than runs of arbitrary whitespace, so a df row
//     padded to align its columns comes back full of empty strings.
//
// Each of these produces a wrong answer only on input the fixtures do not contain,
// which is the worst shape a bug can have here -- the corpus passes, the harness
// reports equivalence, and a real host with a \x1c in a filename gets a different
// answer from the two implementations. Getting it right once, in one package with
// its behaviour pinned against CPython, is cheaper than finding it forty times.
//
// The delta in IsSpace was measured rather than assumed: every code point from 0 to
// 0x10FFFF was run through both str.isspace() and unicode.IsSpace, and the
// difference is exactly U+001C..U+001F, with nothing Go considers space that Python
// does not.
package pytext

import (
	"unicode"
	"unicode/utf8"
)

// IsSpace reports whether r is whitespace to Python's str.isspace().
func IsSpace(r rune) bool {
	// The file, group, record and unit separators. Python's whitespace table
	// includes them; Go's White_Space property does not.
	if r >= 0x1C && r <= 0x1F {
		return true
	}
	return unicode.IsSpace(r)
}

// SplitLines splits s on line boundaries, as Python's str.splitlines() does.
//
// The boundary set is Python's, not the ASCII subset: LF, VT, FF, CR, CRLF as a
// single boundary, the FS/GS/RS separators, NEL (U+0085), and the Unicode LINE and
// PARAGRAPH SEPARATORs. A boundary at the very end of s does not yield a trailing
// empty element, and an empty s yields no elements at all -- both behaviours the
// parsers rely on, since command output ends in a newline and a phantom final ""
// would reach every `if not line: continue` guard as a real row.
func SplitLines(s string) []string {
	out := []string{}
	start := 0
	for i := 0; i < len(s); {
		r, size := decodeRune(s[i:])
		if !isLineBoundary(r) {
			i += size
			continue
		}
		out = append(out, s[start:i])
		i += size
		// CRLF is one boundary, not two.
		if r == '\r' && i < len(s) && s[i] == '\n' {
			i++
		}
		start = i
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func isLineBoundary(r rune) bool {
	switch r {
	case '\n', '\v', '\f', '\r', 0x1C, 0x1D, 0x1E, 0x85, 0x2028, 0x2029:
		return true
	}
	return false
}

// SplitN splits s on runs of whitespace, as Python's str.split(None, maxsplit).
//
// Leading whitespace is discarded and runs collapse, so an aligned column layout
// yields one element per column rather than a string of empties. A negative
// maxsplit means unlimited, matching Python's default of -1.
//
// The one subtlety worth stating: when maxsplit stops the scan, the remainder is
// returned with its leading whitespace stripped but its *trailing* whitespace
// intact -- 'a b c  '.split(None, 1) is ['a', 'b c  ']. That is what lets df's
// six-field POSIX layout keep a mount point containing spaces, and also what makes
// a row with trailing blanks carry them into the mount name in both languages.
func SplitN(s string, maxsplit int) []string {
	out := []string{}
	i := 0
	for {
		// Skip the whitespace run before the next field.
		for i < len(s) {
			r, size := decodeRune(s[i:])
			if !IsSpace(r) {
				break
			}
			i += size
		}
		if i >= len(s) {
			return out
		}
		if maxsplit >= 0 && len(out) == maxsplit {
			out = append(out, s[i:])
			return out
		}
		start := i
		for i < len(s) {
			r, size := decodeRune(s[i:])
			if IsSpace(r) {
				break
			}
			i += size
		}
		out = append(out, s[start:i])
	}
}

// Fields splits s on runs of whitespace with no limit: Python's str.split().
func Fields(s string) []string {
	return SplitN(s, -1)
}

// decodeRune reads the next rune, treating an invalid byte as one byte of ordinary
// content.
//
// utf8.DecodeRuneInString already returns (RuneError, 1) there, and RuneError is
// neither whitespace nor a line boundary, so an undecodable byte is carried through
// into a field rather than splitting one. That matches what Python does with a byte
// that reached a str through surrogateescape.
func decodeRune(s string) (rune, int) {
	return utf8.DecodeRuneInString(s)
}
