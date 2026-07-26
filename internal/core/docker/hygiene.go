package docker

import (
	"encoding/json"
	"math"
	"regexp"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// DfRow is one line of `docker system df --format json`.
type DfRow struct {
	Type             string `json:"type"`
	TotalCount       int64  `json:"total_count"`
	Active           int64  `json:"active"`
	SizeBytes        int64  `json:"size_bytes"`
	ReclaimableBytes int64  `json:"reclaimable_bytes"`
	ReclaimablePct   int64  `json:"reclaimable_pct"`
}

// asciiLetters is [A-Za-z], the class v1 uses for a size's unit.
//
// Written out rather than reused from a shared constant because the two patterns
// below want it under different flags, and under re.IGNORECASE the class stops
// being ASCII: see reReclaimed.
const asciiLetters = `[A-Za-z]`

// asciiLettersFolded is [A-Za-z] as re.IGNORECASE sees it.
//
// Measured, because it is not what either language's documentation suggests.
// Python's IGNORECASE grows the class by four code points -- U+017F LATIN SMALL
// LETTER LONG S, U+212A KELVIN SIGN, U+0130 LATIN CAPITAL LETTER I WITH DOT
// ABOVE and U+0131 LATIN SMALL LETTER DOTLESS I. Go's (?i) folds through
// unicode.SimpleFold, which supplies the first two from a-z and A-Z but leaves
// the dotted and dotless I in orbits of their own, so they have to be named. The
// four are then exactly the difference between the two engines, which is what
// TestReclaimedUnitClassMatchesPythonsIgnorecaseSet asserts.
const asciiLettersFolded = `[A-Za-z\x{0130}\x{0131}]`

var (
	// reReclaimable is v1's _RECLAIMABLE_RE. Anchored at both ends because v1
	// uses .match() for the start and $ for the end; the greedy Space* in front
	// of the end anchor already consumes a trailing newline, so Python's
	// "$ also matches before a final \n" needs no translating here.
	reReclaimable = regexp.MustCompile(`\A` + pytext.Space + `*([0-9.]+` + pytext.Space + `*` + asciiLetters + `*B?)` +
		pytext.Space + `*(?:\((` + pytext.Digit + `+)%\))?` + pytext.Space + `*\z`)

	// reReclaimed is v1's _RECLAIMED_RE, which carries re.IGNORECASE -- and so
	// captures a unit its own consumer cannot read. `Total reclaimed space: 5Kb`
	// spelled with a KELVIN SIGN is captured whole here and then rejected by
	// reSize, which has no IGNORECASE, so v1's answer is 0 rather than 5. That is
	// reproduced rather than fixed, which is the reason asciiLettersFolded exists.
	reReclaimed = regexp.MustCompile(`(?i)Total reclaimed space:` + pytext.Space + `*([0-9.]+` + pytext.Space + `*` + asciiLettersFolded + `*B?)`)

	// reSize matches a whole size token, ASCII digits only: v1 spells the digits
	// [0-9] rather than \d here, so an Arabic-Indic five is not a size in either
	// implementation.
	reSize = regexp.MustCompile(`\A([0-9.]+)(` + asciiLetters + `*B?)\z`)
)

// sizeMultipliers is v1's table, keyed on the upper-cased unit. An unlisted unit
// is bytes, so `5X` and `5ZB` are both five.
var sizeMultipliers = map[string]int64{
	"B":   1,
	"":    1,
	"KB":  1000,
	"MB":  1000 * 1000,
	"GB":  1000 * 1000 * 1000,
	"TB":  1000 * 1000 * 1000 * 1000,
	"KIB": 1024,
	"MIB": 1024 * 1024,
	"GIB": 1024 * 1024 * 1024,
	"TIB": 1024 * 1024 * 1024 * 1024,
}

// ParseSystemDFJSONLines reads `docker system df --format json`, one object per
// line, skipping any line that is not JSON.
//
// v1 calls .get on whatever the line decoded to, so a line holding a JSON scalar,
// array or null raises AttributeError out of the function -- an uncaught crash
// rather than an answer. The same goes for a Size that is a truthy non-string
// (AttributeError from .strip) and a Reclaimable that is (TypeError from
// re.match). All three are skipped here, and none is reachable from output a
// container runtime produces.
//
// Two smaller departures, same reasoning: v1 stores a non-string or explicitly
// null Type verbatim in a field it annotates as str, where Type is "" here; and
// v1's counts are arbitrary-precision, so a count above 2^63-1 reads as itself
// there and as 0 here.
func ParseSystemDFJSONLines(text string) []DfRow {
	rows := []DfRow{}
	for _, line := range pytext.SplitLines(text) {
		line = strings.TrimFunc(line, pytext.IsSpace)
		if line == "" {
			continue
		}
		v, ok := decodeJSONLine(line)
		if !ok {
			continue
		}
		obj, ok := v.(map[string]any)
		if !ok {
			continue
		}
		reclaimable := stringField(obj["Reclaimable"])
		rows = append(rows, DfRow{
			Type:             stringField(obj["Type"]),
			TotalCount:       intOrZero(obj["TotalCount"]),
			Active:           intOrZero(obj["Active"]),
			SizeBytes:        ParseSize(stringField(obj["Size"])),
			ReclaimableBytes: ParseReclaimableBytes(reclaimable),
			ReclaimablePct:   ParseReclaimablePct(reclaimable),
		})
	}
	return rows
}

// ParseReclaimableBytes reads the size out of a `system df` Reclaimable column,
// which docker prints either as "45.68GB (85%)" or as a bare "7.817GB".
//
// The column's own pattern permits whitespace between the number and the unit
// where ParseSize only tolerates U+0020, so "45.68\tGB (85%)" matches here and is
// then read as no size at all. v1 does the same.
func ParseReclaimableBytes(reclaimable string) int64 {
	m := reReclaimable.FindStringSubmatch(reclaimable)
	if m == nil {
		return 0
	}
	return ParseSize(m[1])
}

// ParseReclaimablePct reads the percentage out of the same column, reporting 0
// when docker omitted it -- which is what v1 does, and which is why a real 0%
// and an absent percentage are indistinguishable downstream.
//
// A percentage too large for an int64 is 0 rather than v1's exact number; docker
// prints two digits.
func ParseReclaimablePct(reclaimable string) int64 {
	m := reReclaimable.FindStringSubmatch(reclaimable)
	if m == nil || m[2] == "" {
		return 0
	}
	// Digit+ cannot capture an empty string, so an empty group 2 means the
	// optional percentage did not match, which is v1's `pct is None`.
	pct, err := pytext.Int(m[2])
	if err != nil {
		return 0
	}
	return pct
}

// ParseSize converts a docker size token to bytes.
//
// The tokens are decimal-prefixed ("53.5GB" is 53.5 * 10^9) with the binary
// prefixes accepted too, and the conversion truncates towards zero, so a
// "0.0001KB" is nought bytes. Whitespace is stripped from the ends and U+0020
// removed from the middle -- only U+0020, so a tab or a non-breaking space
// between the number and the unit makes the whole token unreadable.
//
// Two departures, both on tokens v1 cannot answer either: a number that
// overflows a float64 (400 nines) makes v1's int(inf) raise OverflowError
// uncaught, and a product above 2^63-1 is exact there and 0 here.
func ParseSize(value string) int64 {
	value = strings.ReplaceAll(strings.TrimFunc(value, pytext.IsSpace), " ", "")
	if value == "" {
		return 0
	}
	m := reSize.FindStringSubmatch(value)
	if m == nil {
		return 0
	}
	// float() raises on a run of dots that is not a number; v1 catches it.
	number, err := pytext.Float(m[1])
	if err != nil {
		return 0
	}
	multiplier, ok := sizeMultipliers[strings.ToUpper(m[2])]
	if !ok {
		multiplier = 1
	}
	return truncate(number * float64(multiplier))
}

// ParseReclaimedTotal reads the trailer `docker system prune` prints, reporting 0
// when there is none. Only the first occurrence is read, matching v1's search.
func ParseReclaimedTotal(text string) int64 {
	m := reReclaimed.FindStringSubmatch(text)
	if m == nil {
		return 0
	}
	return ParseSize(m[1])
}

// stringField reads a `system df` field v1 defaults to "".
//
// Only a string is a value here: v1's default applies to an absent key alone, so
// a present null or a non-string reaches the size parsers as itself and either
// crashes v1 or is stored unvalidated. Both are documented on
// ParseSystemDFJSONLines.
func stringField(v any) string {
	s, _ := v.(string)
	return s
}

// intOrZero is v1's _int_or_zero: int(value), or 0 when int() will not have it.
//
// The type switch is what that one line means once the JSON is decoded. A string
// goes through Python's int(), underscores, Unicode digits, surrounding
// whitespace and all -- but not "3.9", which raises there and is 0 here. A JSON
// number is an int literal if int() would take it and otherwise a float
// truncated towards zero, so 3.9 is 3 and -3.9 is -3. A bool is 1 or 0, because
// Python's bool is an int. Everything else, null and containers included, is a
// TypeError v1 catches.
func intOrZero(v any) int64 {
	switch t := v.(type) {
	case string:
		n, err := pytext.Int(t)
		if err != nil {
			return 0
		}
		return n
	case json.Number:
		if n, err := pytext.Int(string(t)); err == nil {
			return n
		}
		f, err := pytext.Float(string(t))
		if err != nil {
			return 0
		}
		return truncate(f)
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// truncate is int() on a float: towards zero, and 0 for the values Python's int()
// refuses or answers with a number no int64 holds.
func truncate(f float64) int64 {
	t := math.Trunc(f)
	// -float64(math.MinInt64) is 2^63 exactly, so the comparison also rejects
	// +Inf, and NaN fails every comparison and has to be asked about separately.
	if math.IsNaN(t) || t < math.MinInt64 || t >= -float64(math.MinInt64) {
		return 0
	}
	return int64(t)
}
