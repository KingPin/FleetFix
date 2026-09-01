package config

// Python's bool() and str() over this package's value vocabulary.
//
// They live here because the vocabulary does: a caller that reads a config value
// gets one of the eleven types listed in the package doc, and v1 fed those values
// straight to bool() and str(). audit/otel.py does both -- bool(data.get("insecure",
// False)) and str(data.get("service_name") or DEFAULT) -- and the coercions are not
// incidental. `insecure: "false"` is a non-empty string and therefore True, which is
// the opposite of what the operator who quoted it meant, and reproducing that is the
// difference between their existing otel.yml behaving as it did and behaving as it
// reads.

import (
	"fmt"
	"maps"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// PyTruthy reports what Python's bool() would return for v.
//
// Empty containers and strings are false, zero numbers are false, and everything
// else is true -- including NaN, which is not zero.
func PyTruthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case int64:
		return t != 0
	case *big.Int:
		return t.Sign() != 0
	case float64:
		return t != 0
	case string:
		return t != ""
	case []byte:
		return len(t) != 0
	case time.Time:
		// A datetime is always truthy. (datetime.time at midnight was falsy before
		// Python 3.5; datetime never was.)
		return true
	case []any:
		return len(t) != 0
	case map[string]any:
		return len(t) != 0
	case map[string]struct{}:
		return len(t) != 0
	default:
		// An object with no __bool__ and no __len__ is true.
		return true
	}
}

// PyStr renders v the way Python's str() would.
//
// Exact for None, bool, int, float, str and datetime -- the types an operator can
// plausibly leave unquoted where a string is wanted, which is the whole reason this
// exists: `headers: {x-token: 12345}` must come out "12345", not be dropped for not
// being a string.
//
// Collections and bytes defer to PyRepr, because str() of a container calls repr()
// on it. That matters somewhere real: probes.yml turns every entry of a target list
// into a string, so one extra dash in the YAML makes a list the thing being rendered,
// and v1 puts "['8.8.8.8']" in the target slot rather than dropping it.
//
// Exact for everything except the three shapes PyRepr cannot promise.
func PyStr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case int64:
		return strconv.FormatInt(t, 10)
	case *big.Int:
		return t.String()
	case float64:
		// str() and repr() agree for floats, and PyJSONFloat is repr for every finite
		// one. The three non-finite spellings are where JSON and Python part company:
		// json.dumps writes Infinity, str() writes inf.
		switch {
		case math.IsNaN(t):
			return "nan"
		case math.IsInf(t, 1):
			return "inf"
		case math.IsInf(t, -1):
			return "-inf"
		}
		return PyJSONFloat(t)
	case string:
		return t
	case time.Time:
		// str(datetime) is isoformat with a space instead of the T, microseconds only
		// when non-zero, and a numeric offset rather than Z.
		layout := "2006-01-02 15:04:05-07:00"
		if t.Nanosecond() != 0 {
			layout = "2006-01-02 15:04:05.000000-07:00"
		}
		return t.Format(layout)
	case []byte, []any, map[string]any, map[string]struct{}:
		return PyRepr(v)
	default:
		return fmt.Sprint(v)
	}
}

// PyRepr renders v the way Python's repr() would.
//
// repr and str agree on every scalar this package produces except str and bytes,
// where repr adds the quotes and the escapes -- and those are what a container's
// str() shows, because str(list) reprs its elements.
//
// Three shapes this cannot promise:
//
//   - A mapping and a set. Python renders them in iteration order, which for a dict
//     is insertion order and for a set is hash order; Go's map has neither, so both
//     come out sorted by key. Sorted is stable and honest about it, where Go's own
//     map ordering would be randomised per run and would turn a config value into a
//     flaky one.
//   - A timestamp. repr(datetime) is the constructor call --
//     "datetime.datetime(2026, 7, 26, 0, 0)" -- and a bare YAML date reprs as
//     datetime.date instead, a distinction time.Time does not keep. It strs instead,
//     which is the isoformat.
//
// All three are only reachable through a container, because that is the only place
// str() calls repr(). A scalar config value never takes this path.
func PyRepr(v any) string {
	switch t := v.(type) {
	case string:
		return pyQuoteStr(t)
	case []byte:
		return pyQuoteBytes(t)
	case []any:
		if len(t) == 0 {
			return "[]"
		}
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = PyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		if len(t) == 0 {
			return "{}"
		}
		parts := make([]string, 0, len(t))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			parts = append(parts, pyQuoteStr(k)+": "+PyRepr(t[k]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]struct{}:
		// An empty set is set(), not {} -- {} is an empty dict, and Python has no
		// literal for the empty set.
		if len(t) == 0 {
			return "set()"
		}
		parts := make([]string, 0, len(t))
		for _, k := range slices.Sorted(maps.Keys(t)) {
			parts = append(parts, pyQuoteStr(k))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		// Every remaining type in the vocabulary reprs the way it strs.
		return PyStr(v)
	}
}

// pyQuoteStr is Python's repr of a str.
func pyQuoteStr(s string) string {
	quote := pyQuoteChar(s)
	var b strings.Builder
	b.WriteByte(quote)
	for _, r := range s {
		switch {
		case r == rune(quote) || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case !unicode.IsPrint(r):
			// Go's IsPrint is Python's str.isprintable(): categories L, M, N, P, S
			// plus ASCII space, which is why U+00A0 escapes and U+00E9 does not.
			// Python 3's repr emits printable non-ASCII literally.
			switch {
			case r < 0x100:
				fmt.Fprintf(&b, `\x%02x`, r)
			case r < 0x10000:
				fmt.Fprintf(&b, `\u%04x`, r)
			default:
				fmt.Fprintf(&b, `\U%08x`, r)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyQuoteBytes is Python's repr of a bytes.
//
// Byte-wise, not rune-wise: bytes repr is ASCII plus hex escapes, and !!binary is
// base64 of arbitrary bytes, so decoding it as UTF-8 first would render a blob that
// is not text as something other than what Python shows.
func pyQuoteBytes(p []byte) string {
	quote := pyQuoteChar(string(p))
	var b strings.Builder
	b.WriteString("b")
	b.WriteByte(quote)
	for _, c := range p {
		switch {
		case c == quote || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20 || c >= 0x7f:
			fmt.Fprintf(&b, `\x%02x`, c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte(quote)
	return b.String()
}

// pyQuoteChar picks repr's quote: single, unless the value holds one and no double
// quote. That rule is what makes repr("it's") come out "it's" rather than 'it\'s'.
func pyQuoteChar(s string) byte {
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		return '"'
	}
	return '\''
}

// PyJSONFloat spells a float the way Python's json.dumps writes it: repr for
// every finite value, and the JavaScript-flavoured Infinity/NaN words for the
// rest -- which is Python's non-standard extension, not JSON.
//
// Exported, unlike the rest of this file's helpers, because the audit trail is
// the other place a Go process has to produce bytes a Python one would have
// produced. Two spellings of the same float in one repository would be two
// answers to "did the trail change across the upgrade?".
func PyJSONFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	// Python's repr switches to exponential outside [1e-4, 1e16); Go's %g picks
	// its own thresholds, so the choice is made here instead.
	if abs := math.Abs(f); abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}
