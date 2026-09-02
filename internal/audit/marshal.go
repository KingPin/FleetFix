package audit

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/config"
)

// marshalRecord renders one record as the line v1 would have written, newline
// included.
//
// Hand-written rather than encoding/json, and the reason is bytes. v1 called
// json.dumps(record, ensure_ascii=False), and that differs from Go's marshaller
// in four ways, every one of which shows up in a diff of two hosts' trails
// across the Python-to-Go upgrade:
//
//   - Separators. Python's defaults are ", " and ": ", with the spaces. Go
//     writes "," and ":". Every line differs.
//   - HTML escaping. Go escapes <, > and & to their \u003c form by default; an
//     asset URL with a query string would come out unreadable. Disableable, and
//     disabled everywhere else in this repository, but see the next two.
//   - \b and \f. Python has short escapes for both; Go writes \u0008 and
//     \u000c even with SetEscapeHTML off.
//   - U+2028 and U+2029. Go always escapes them, as a JavaScript-embedding
//     workaround. Python with ensure_ascii=False writes them through.
//
// The last two have no switch, so there is no configuration of encoding/json
// that produces these bytes.
func marshalRecord(rec Record) []byte {
	var b strings.Builder
	b.WriteByte('{')
	writeMember(&b, "ts", rec.Timestamp, true)
	writeMember(&b, "host", rec.Host, false)
	writeMember(&b, "session_id", rec.SessionID, false)
	writeMember(&b, "call_id", rec.CallID, false)
	writeMember(&b, "seq", rec.Seq, false)
	writeMember(&b, "phase", rec.Phase, false)
	writeMember(&b, "operator", Fields{
		{Key: "unix_user", Value: rec.Operator.UnixUser},
		{Key: "auth_principal", Value: emptyAsNull(rec.Operator.AuthPrincipal)},
		{Key: "source_ip", Value: emptyAsNull(rec.Operator.SourceIP)},
	}, false)
	writeMember(&b, "inspect_target", emptyAsNull(rec.InspectTarget), false)
	writeMember(&b, "action", rec.Action, false)
	writeMember(&b, "target", rec.Target, false)
	if rec.HasResult {
		writeMember(&b, "result", rec.Result, false)
	} else {
		writeMember(&b, "result", nil, false)
	}
	writeMember(&b, "fleetfix_version", rec.Version, false)
	b.WriteString("}\n")
	return []byte(b.String())
}

// emptyAsNull maps identity's "" for absent onto the null v1 wrote.
//
// The two vocabularies differ on purpose. Inside the process, an empty string
// and an unset auth principal are the same fact and a pointer would only add a
// nil check at every use. On the wire they are not: a consumer counting records
// with an SSO identity has to be able to tell "nobody configured a mapper" from
// "the mapper ran and produced nothing", and null is the answer v1 gave.
func emptyAsNull(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// writeMember writes one key/value pair, prefixing the Python separator unless
// it is the first.
func writeMember(b *strings.Builder, key string, value any, first bool) {
	if !first {
		b.WriteString(", ")
	}
	writeString(b, key)
	b.WriteString(": ")
	writeValue(b, value)
}

// writeValue renders one value in Python's json.dumps vocabulary.
//
// The default branch renders an unexpected type with %v rather than failing or
// dropping the field. A record is written at the moment something is being done
// to a host, and the two alternatives are worse in both directions: dropping
// loses the only evidence, and failing turns a mis-typed field into a refused
// destructive action.
func writeValue(b *strings.Builder, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString(strconv.FormatBool(t))
	case string:
		writeString(b, t)
	case int:
		b.WriteString(strconv.Itoa(t))
	case int64:
		b.WriteString(strconv.FormatInt(t, 10))
	case float64:
		b.WriteString(config.PyJSONFloat(t))
	case Fields:
		writeFields(b, t)
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			writeValue(b, e)
		}
		b.WriteByte(']')
	case []string:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteString(", ")
			}
			writeString(b, e)
		}
		b.WriteByte(']')
	default:
		writeString(b, fmt.Sprintf("%v", t))
	}
}

func writeFields(b *strings.Builder, f Fields) {
	b.WriteByte('{')
	for i, m := range f {
		writeMember(b, m.Key, m.Value, i == 0)
	}
	b.WriteByte('}')
}

// writeString is Python's json.dumps escaping with ensure_ascii=False.
//
// The escape set is exactly CPython's: backslash, double quote, the five short
// control escapes, and \u00xx for every other character below 0x20. Everything
// at or above 0x20 is written through as UTF-8, non-ASCII included -- which is
// why a hostname or a path with a non-Latin character survives the trail rather
// than turning into a run of \uXXXX nobody can grep for.
func writeString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		case c == '\b':
			b.WriteString(`\b`)
		case c == '\f':
			b.WriteString(`\f`)
		case c == '\n':
			b.WriteString(`\n`)
		case c == '\r':
			b.WriteString(`\r`)
		case c == '\t':
			b.WriteString(`\t`)
		case c < 0x20:
			// Lowercase hex, four digits: CPython's "\\u{0:04x}".
			b.WriteString(`\u00`)
			const hexDigits = "0123456789abcdef"
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		default:
			// Byte at a time, so invalid UTF-8 passes through unchanged instead
			// of becoming U+FFFD. Python's str cannot hold invalid UTF-8 at all,
			// so there is no v1 behaviour to match -- but a path Go read off a
			// filesystem can, and substituting a replacement character would put
			// a name in the trail that does not name the file that was deleted.
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}
