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
	"math"
	"math/big"
	"strconv"
	"time"
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
// Not exact for []byte and the three collection types: v1 would spell those with
// Python's repr ("b'hi'", "[1, 2]", "{'a': 1}") and reproducing that is a pile of
// escaping rules for a config file nobody writes. They get a Go rendering instead,
// which is wrong in the same direction v1 was useless in -- an OTLP header whose
// value is a nested mapping was never going to work either way.
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
		// str() and repr() agree for floats, and jsonFloat is repr for every finite
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
		return jsonFloat(t)
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
	default:
		return fmt.Sprint(v)
	}
}
