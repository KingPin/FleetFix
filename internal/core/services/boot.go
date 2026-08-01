package services

import (
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// BlameEntry is one unit's contribution to the last boot, from
// `systemd-analyze blame`.
//
// The duration is milliseconds because that is the finest unit systemd prints
// and because an integer keeps the sort total: two units that took the same
// number of milliseconds must not reorder between runs on a float comparison.
type BlameEntry struct {
	Unit       string `json:"unit"`
	DurationMS int64  `json:"duration_ms"`
}

// The three duration tokens systemd-analyze prints, and the reason the seconds
// one looks nothing like v1's.
//
// v1's seconds pattern is `(?<![\d.])([\d.]+)s\b`, and RE2 has no lookbehind. It
// is reproduced by *consuming* the preceding character instead of asserting it,
// which is safe here rather than merely close: a match must begin at the start of
// a maximal [\d.] run either way, so the set of positions group 1 can start at is
// identical, and Go's regexp picks a match the same leftmost-first way Python's
// does. The alternation order matters -- ^ is tried first so that a token
// starting with a digit yields group 1 at offset 0 rather than skipping to the
// second digit.
//
// The trailing (?:[^\p{L}\p{N}_]|$) is \b after an "s": Python's \w on str is
// [\p{L}\p{N}_], so a word boundary there is exactly "the next character is not
// one of those, or there is no next character".
//
// Both character classes are [\p{Nd}] rather than [0-9] because Python's \d on
// str is Nd, and float()/int() accept those digits -- "٥s" really is five
// seconds to v1, so it has to be five seconds here.
var (
	reMinutes      = regexp.MustCompile(`([\p{Nd}]+)min`)
	reSeconds      = regexp.MustCompile(`(?:^|[^\p{Nd}.])([\p{Nd}.]+)s(?:[^\p{L}\p{N}_]|$)`)
	reMilliseconds = regexp.MustCompile(`([\p{Nd}.]+)ms(?:[^\p{L}\p{N}_]|$)`)
)

// ParseBlame parses `systemd-analyze blame`, slowest unit first as systemd
// already sorted it.
//
// Each line is a duration followed by a unit name. The unit is the last
// whitespace-separated field and everything before it is the duration, which is
// why a line with no whitespace at all is skipped: v1 unpacks a two-element
// rsplit and a one-element result raises, so such a line never became an entry.
//
// A line whose duration names no recognised unit of time is skipped too. That
// covers the header-ish and truncated lines, and also a bare number -- "5" with
// no suffix is not five of anything.
func ParseBlame(text string) []BlameEntry {
	out := []BlameEntry{}
	for _, line := range pytext.SplitLines(text) {
		line = strings.TrimFunc(line, pytext.IsSpace)
		if line == "" {
			continue
		}
		timePart, unit, ok := rsplitOnce(line)
		if !ok {
			continue
		}
		ms, ok := parseTime(timePart)
		if !ok {
			continue
		}
		out = append(out, BlameEntry{Unit: unit, DurationMS: ms})
	}
	return out
}

// rsplitOnce is Python's rsplit(None, maxsplit=1) on a string with no leading or
// trailing whitespace: it cuts at the last run of whitespace and reports whether
// there was one to cut at.
//
// Splitting on None means runs, not single characters, so "5s  foo" yields "5s"
// and not "5s ". The whitespace set is Python's str.isspace(), so a non-breaking
// space separates a duration from its unit name here exactly as it does in
// CPython.
func rsplitOnce(s string) (left, right string, ok bool) {
	i := strings.LastIndexFunc(s, pytext.IsSpace)
	if i < 0 {
		return "", "", false
	}
	_, size := utf8.DecodeRuneInString(s[i:])
	return strings.TrimRightFunc(s[:i], pytext.IsSpace), s[i+size:], true
}

// parseTime sums the minutes, seconds and milliseconds named in a duration
// token, reporting whether it named any of them.
//
// The three searches are independent and each takes its first match, which is
// what makes "1min30s" ninety seconds and "5s6s" six: the seconds search cannot
// match "5s" there because a digit follows the s, so it finds "6s" instead. That
// is v1's behaviour and not obviously anyone's intent, but systemd does not print
// either shape, so nothing real depends on which reading is right.
//
// v1 also guards against an empty token. That guard is unreachable from
// ParseBlame -- the line is stripped and non-empty, so the part before the last
// whitespace run is non-empty too -- and it is redundant with the "named
// something" answer anyway, so it is not reproduced.
//
// Two failures are Go's alone and both report "no duration", i.e. the line is
// dropped:
//
//   - A capture like "1.2.3" or "." matches [\p{Nd}.]+ but is not a number.
//     float() raises ValueError in v1 and the exception escapes parse_blame
//     uncaught, so v1's answer to such a line is a traceback. Dropping the line
//     is the closest thing to an answer; reproducing a crash is not one.
//   - A duration past 2^63-1 milliseconds. v1's ints are arbitrary precision, so
//     it reports the real number; that takes sixteen digits of seconds or twenty
//     of minutes, which is 300 million years of boot and cannot come from
//     systemd.
//
// Neither is reachable from any fixture in the corpus, which is why there is no
// known_divergences.yaml entry for them.
func parseTime(token string) (int64, bool) {
	var total int64
	matched := false

	// Every term is non-negative -- neither pattern can capture a sign -- so an
	// overflowing sum is a sum that got smaller, and each product only has to be
	// checked against the one bound.
	add := func(n int64) bool {
		if total+n < total {
			return false
		}
		total += n
		return true
	}

	if m := reMinutes.FindStringSubmatch(token); m != nil {
		n, err := pytext.Int(m[1])
		if err != nil || n > math.MaxInt64/60_000 || !add(n*60_000) {
			return 0, false
		}
		matched = true
	}
	if m := reSeconds.FindStringSubmatch(token); m != nil {
		f, err := pytext.Float(m[1])
		if err != nil {
			return 0, false
		}
		n, ok := truncate(f * 1000)
		if !ok || !add(n) {
			return 0, false
		}
		matched = true
	}
	if m := reMilliseconds.FindStringSubmatch(token); m != nil {
		f, err := pytext.Float(m[1])
		if err != nil {
			return 0, false
		}
		n, ok := truncate(f)
		if !ok || !add(n) {
			return 0, false
		}
		matched = true
	}

	if !matched {
		return 0, false
	}
	return total, true
}

// truncate is int() on a float: it drops the fractional part towards zero, the
// way both languages do, and reports whether the result is an int64 at all.
//
// Python's int() has no upper bound but raises OverflowError on an infinity, and
// float("9"*400) is an infinity, so "no answer" is the honest report for both
// the infinite and the merely enormous case.
func truncate(f float64) (int64, bool) {
	t := math.Trunc(f)
	if math.IsNaN(t) || t < math.MinInt64 || t >= -float64(math.MinInt64) {
		return 0, false
	}
	return int64(t), true
}
