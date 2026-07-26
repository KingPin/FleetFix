// Package pytime is Python's datetime semantics, the way internal/pytext is
// Python's str semantics: the pieces of the standard library v1 leans on that
// Go's equivalents do not spell the same way.
//
// Only one thing lives here, and one is enough to earn a package. v1 reads
// docker's container start time with datetime.fromisoformat, which is not
// time.RFC3339 with a different name -- it accepts ISO week dates, basic-format
// dates and times, arbitrary fractional-second precision, sub-minute UTC
// offsets, hour 24 as tomorrow's midnight, and any single character as the
// date/time separator, while rejecting a lowercase "z" and a leading space.
// Nothing in Go's time package draws that boundary, so the boundary is drawn
// here where it can be tested against the interpreter.
package pytime

import (
	"errors"
	"strings"
	"time"
)

// ErrISOFormat is returned for input datetime.fromisoformat would reject with a
// ValueError. There is one error rather than one per rule because the caller has
// one decision to make: v1's _parse_iso catches ValueError and reports None, and
// no consumer ever learns which rule was broken.
var ErrISOFormat = errors.New("pytime: invalid isoformat string")

// Time is what datetime.fromisoformat returns: a wall-clock reading plus, when
// the text carried one, an offset from UTC.
//
// The two are kept apart rather than folded into a time.Time because Python
// keeps them apart, and the difference is load-bearing. A datetime with no
// tzinfo is *naive* -- it names a wall clock without saying whose -- and
// subtracting an aware datetime from a naive one is a TypeError, not a
// timestamp. v1's Container.is_restart_loop does exactly that subtraction
// against datetime.now(timezone.utc), so a container whose StartedAt somehow
// arrived without an offset takes the docker view down. Folding naive into UTC
// here would hide that, and hiding it is how the collectors end up quietly
// answering a question nobody asked.
//
// Offset is meaningful only when Aware. It can carry sub-second precision
// because Python's timezone() does: "+05:30:45.123456" is a legal offset to
// fromisoformat, which is why this is a Duration and not the seconds count
// time.FixedZone would take.
type Time struct {
	Year        int           `json:"year"`
	Month       int           `json:"month"`
	Day         int           `json:"day"`
	Hour        int           `json:"hour"`
	Minute      int           `json:"minute"`
	Second      int           `json:"second"`
	Microsecond int           `json:"microsecond"`
	Offset      time.Duration `json:"offset"`
	Aware       bool          `json:"aware"`
}

// Wall is the reading as it was written, with no offset applied. The location is
// UTC because a Go time.Time must have one, not because the reading is UTC --
// check Aware before believing it names an instant.
func (t Time) Wall() time.Time {
	return time.Date(t.Year, time.Month(t.Month), t.Day, t.Hour, t.Minute, t.Second,
		t.Microsecond*1000, time.UTC)
}

// Instant is the moment this reading names, which takes an offset to work out.
// ok is false for a naive reading, where there is no answer -- see Time.
func (t Time) Instant() (_ time.Time, ok bool) {
	if !t.Aware {
		return time.Time{}, false
	}
	return t.Wall().Add(-t.Offset), true
}

// ISOFormat is Python's datetime.isoformat(): the round trip back to text, with
// microseconds present only when non-zero and the offset spelled to the finest
// unit it uses.
//
// This exists because it is the wire form. The differential harness compares
// what the oracle emits, and the oracle reduces a datetime with isoformat(), so
// this is the function whose output has to match Python character for character
// -- a datetime that parsed correctly and rendered "+05:30" where Python wrote
// "+05:30:45" is still a divergence.
func (t Time) ISOFormat() string {
	var b strings.Builder
	writePadded(&b, t.Year, 4)
	b.WriteByte('-')
	writePadded(&b, t.Month, 2)
	b.WriteByte('-')
	writePadded(&b, t.Day, 2)
	b.WriteByte('T')
	writePadded(&b, t.Hour, 2)
	b.WriteByte(':')
	writePadded(&b, t.Minute, 2)
	b.WriteByte(':')
	writePadded(&b, t.Second, 2)
	if t.Microsecond != 0 {
		b.WriteByte('.')
		writePadded(&b, t.Microsecond, 6)
	}
	if !t.Aware {
		return b.String()
	}
	off := t.Offset
	if off < 0 {
		b.WriteByte('-')
		off = -off
	} else {
		b.WriteByte('+')
	}
	writePadded(&b, int(off/time.Hour), 2)
	b.WriteByte(':')
	writePadded(&b, int(off/time.Minute)%60, 2)
	// Seconds and microseconds appear only when the offset uses them, which is
	// Python's rule and not merely a formatting preference: the common case has to
	// come back out as "+00:00" and not "+00:00:00".
	if secs, us := int(off/time.Second)%60, int(off/time.Microsecond)%1_000_000; secs != 0 || us != 0 {
		b.WriteByte(':')
		writePadded(&b, secs, 2)
		if us != 0 {
			b.WriteByte('.')
			writePadded(&b, us, 6)
		}
	}
	return b.String()
}

// writePadded writes n zero-padded to width digits. Every caller passes a value
// already validated into range, so there is no negative or overlong case to
// handle -- and a bug that produced one would show up as a wrong string in the
// isoformat golden rather than being papered over here.
func writePadded(b *strings.Builder, n, width int) {
	digits := [6]byte{}
	for i := width - 1; i >= 0; i-- {
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	b.Write(digits[:width])
}

// FromISOFormat parses one of the ISO 8601 formats datetime.fromisoformat
// accepts, which is a good deal more than time.RFC3339 and rather less than all
// of ISO 8601:
//
//	date       YYYY-MM-DD | YYYYMMDD | YYYY-Www[-D] | YYYYWww[D]
//	separator  any single character, so "T", " ", "#" and "😀" all work
//	time       HH[:MM[:SS[{.|,}f+]]], or the same without the colons
//	offset     Z | ±HH[:MM[:SS[.f+]]], or the same without the colons
//
// with these details, each measured against the interpreter rather than read off
// the documentation:
//
//   - Digits are ASCII only. int() accepts every Unicode Nd digit, so
//     pytext.Int reads "٥" as 5, but this does not: "٢٠٢٦-٠٧-٢٦" is a ValueError.
//   - Fractional seconds may run to any length; the first six digits are used
//     and the rest are discarded, not rounded. Docker writes nine.
//   - Hour 24 is tomorrow's midnight, and only when minute, second and
//     microsecond are all zero.
//   - Offset components are not range-checked individually -- "+05:60" is six
//     hours -- but the total must be under 24 hours, so "+23:60" is not an offset.
//   - An all-zero offset is UTC whichever sign it carried, so "-00:00" is "+00:00".
//   - Dash use inside a date must be consistent: "2026-0726" and "202607-26" are
//     both rejected. Colon use inside a time likewise.
//   - Uppercase "Z" only, and no surrounding whitespace: " 2026-07-26" and
//     "2026-07-26T15:04:05z" are both ValueErrors.
//
// There is a departure, and it is CPython disagreeing with itself rather than
// with Go. Every interpreter carries two implementations of this function -- the
// C accelerator that actually runs, and the pure-Python reference in
// _pydatetime.py -- and a mutation sweep over 26,413 candidate strings put them
// at odds on 330. C rejects 186 that the reference accepts, mostly because the
// reference parses components with int() and so inherits its tolerance for
// leading whitespace and a leading '+' (" 026-07-26" is year 26 to the
// reference); a separator with no time after it, "2026-07-26T", is another.
// C accepts 144 that the reference rejects, all malformed: extra digits or a
// stray separator immediately before the timezone marker ("15:04:005Z" reads as
// 15:04:00, "15:+04:05" as 15:00+04:05), and a NUL in certain positions.
//
// The rule here is to reject every input the two disagree about, which is the
// intersection of what they accept and needs no case-by-case judgement. None of
// the 330 is reachable from the one caller: docker and podman both render
// .State.StartedAt through Go's own RFC3339Nano.
func FromISOFormat(s string) (Time, error) {
	// Runes, not bytes: Python indexes the string by code point, so the separator
	// slot holds one character however many bytes it takes, and the length checks
	// that pick a date shape count characters too.
	r := []rune(s)
	if len(r) < 7 {
		return Time{}, ErrISOFormat
	}
	sep, err := findDateTimeSeparator(r)
	if err != nil {
		return Time{}, err
	}
	// The separator search infers a shape from the string's length and can point
	// past the end of a string that does not have one -- "\x2019-1Z7Z" resolves to
	// 10 with nine characters to show for it. CPython asserts here, which is to
	// say it does not expect to get this far; reject it.
	if sep > len(r) {
		return Time{}, ErrISOFormat
	}
	year, month, day, err := parseDate(r[:sep])
	if err != nil {
		return Time{}, err
	}

	var t Time
	if sep < len(r) {
		var nextDay bool
		if t, nextDay, err = parseTime(r[sep+1:]); err != nil {
			return Time{}, err
		}
		if nextDay {
			// Only after the date it applies to is known good: "2026-13-01T24:00"
			// is a bad month, not tomorrow.
			if err := validateDate(year, month, day); err != nil {
				return Time{}, err
			}
			year, month, day = dayAfter(year, month, day)
		}
	}
	t.Year, t.Month, t.Day = year, month, day
	if err := validateDate(year, month, day); err != nil {
		return Time{}, err
	}
	return t, nil
}

// findDateTimeSeparator works out how many leading characters are the date, which
// is not something a scan for "T" can answer: the separator may be any character,
// so the date's own length has to give it away. Ported from CPython's
// _find_isoformat_datetime_separator, ambiguity and all -- "2026-W30-12" could be
// week 30 day 1 followed by the separator "2", or week 30 followed by "-12", and
// CPython picks the first because a hyphen is the likelier separator.
func findDateTimeSeparator(r []rune) (int, error) {
	if len(r) == 7 {
		return 7, nil
	}
	if r[4] == '-' {
		if r[5] != 'W' {
			return 10, nil // YYYY-MM-DD
		}
		if len(r) > 8 && r[8] == '-' {
			if len(r) == 9 {
				return 0, ErrISOFormat
			}
			if len(r) > 10 && isASCIIDigit(r[10]) {
				return 8, nil
			}
			return 10, nil // YYYY-Www-D
		}
		return 8, nil // YYYY-Www
	}
	if r[4] != 'W' {
		return 8, nil // YYYYMMDD
	}
	// YYYYWww or YYYYWwwD, told apart by where the digits stop: an even index
	// means the last of them was the day number.
	idx := 7
	for idx < len(r) && isASCIIDigit(r[idx]) {
		idx++
	}
	if idx < 9 {
		return idx, nil
	}
	if idx%2 == 0 {
		return 7, nil
	}
	return 8, nil
}

// parseDate reads the date half, which the separator search has already sized to
// 7, 8 or 10 characters -- anything else is a string that has no ISO date shape.
func parseDate(d []rune) (year, month, day int, err error) {
	if len(d) != 7 && len(d) != 8 && len(d) != 10 {
		return 0, 0, 0, ErrISOFormat
	}
	if year, err = fixedDigits(d, 0, 4); err != nil {
		return 0, 0, 0, err
	}
	hasSep := 0
	if d[4] == '-' {
		hasSep = 1
	}
	pos := 4 + hasSep

	if pos < len(d) && d[pos] == 'W' {
		pos++
		week, err := fixedDigits(d, pos, 2)
		if err != nil {
			return 0, 0, 0, err
		}
		pos += 2
		weekday := 1
		if len(d) > pos {
			// The dash before the day number is present exactly when the one after
			// the year was. Half-extended dates are not dates.
			if (d[pos] == '-') != (hasSep == 1) {
				return 0, 0, 0, ErrISOFormat
			}
			pos += hasSep
			if weekday, err = fixedDigits(d, pos, 1); err != nil {
				return 0, 0, 0, err
			}
		}
		return isoWeekToGregorian(year, week, weekday)
	}

	if month, err = fixedDigits(d, pos, 2); err != nil {
		return 0, 0, 0, err
	}
	pos += 2
	if (pos < len(d) && d[pos] == '-') != (hasSep == 1) {
		return 0, 0, 0, ErrISOFormat
	}
	pos += hasSep
	if day, err = fixedDigits(d, pos, 2); err != nil {
		return 0, 0, 0, err
	}
	return year, month, day, nil
}

// parseTime reads everything after the separator: the clock, then the offset if
// there is one. nextDay reports hour 24, which the caller turns into tomorrow.
func parseTime(t []rune) (out Time, nextDay bool, err error) {
	if len(t) < 2 {
		return Time{}, false, ErrISOFormat
	}
	// The offset marker is the first '-' anywhere, else the first '+', else the
	// first 'Z' -- in that order, not in the order they appear. CPython spells this
	// as a chain of finds and the precedence falls out of it, so "15:04:05+05-30"
	// splits at the dash and fails rather than reading as a +05 offset.
	tzPos := -1
	for _, marker := range []rune{'-', '+', 'Z'} {
		if i := indexRune(t, marker); i >= 0 {
			tzPos = i + 1
			break
		}
	}
	clock := t
	if tzPos >= 0 {
		clock = t[:tzPos-1]
	}
	hour, minute, second, microsecond, err := parseHHMMSSFF(clock)
	if err != nil {
		return Time{}, false, err
	}
	if hour == 24 {
		if minute != 0 || second != 0 || microsecond != 0 {
			return Time{}, false, ErrISOFormat
		}
		hour, nextDay = 0, true
	}
	if hour > 23 || minute > 59 || second > 59 {
		return Time{}, false, ErrISOFormat
	}
	out = Time{Hour: hour, Minute: minute, Second: second, Microsecond: microsecond}

	switch {
	case tzPos < 0:
		return out, nextDay, nil
	case tzPos == len(t) && t[len(t)-1] == 'Z':
		out.Aware = true
		return out, nextDay, nil
	}
	tz := t[tzPos:]
	// The legal offset lengths are 2, 4, 5, 6, 7+, 8 and 10+; 0, 1 and 3 are
	// nothing, and a marker of 'Z' with anything after it is not an offset either.
	if len(tz) == 0 || len(tz) == 1 || len(tz) == 3 || t[tzPos-1] == 'Z' {
		return Time{}, false, ErrISOFormat
	}
	oh, om, os, ous, err := parseHHMMSSFF(tz)
	if err != nil {
		return Time{}, false, err
	}
	if oh == 0 && om == 0 && os == 0 && ous == 0 {
		// timezone(timedelta(0)) is timezone.utc whatever sign was written.
		out.Aware = true
		return out, nextDay, nil
	}
	off := time.Duration(oh)*time.Hour + time.Duration(om)*time.Minute +
		time.Duration(os)*time.Second + time.Duration(ous)*time.Microsecond
	if t[tzPos-1] == '-' {
		off = -off
	}
	// Python's timezone() takes strictly less than a day either way, which is the
	// only bound on an offset: the components themselves are free, so "+05:60" is
	// six hours and "+23:60" is not an offset at all.
	if off <= -24*time.Hour || off >= 24*time.Hour {
		return Time{}, false, ErrISOFormat
	}
	out.Offset, out.Aware = off, true
	return out, nextDay, nil
}

// fractionScale turns 1..5 digits of fractional seconds into microseconds. Six
// digits already are microseconds, and past six the extra digits are dropped
// rather than rounded -- docker writes nine, so ".123456789" is 123456µs and the
// 789 nanoseconds are gone. Python does the same; it is the one place where this
// silently loses precision v1 also lost.
var fractionScale = [5]int{100000, 10000, 1000, 100, 10}

// parseHHMMSSFF reads HH[:?MM[:?SS[{.|,}f+]]], which is the clock and the offset
// both -- Python parses them with one function, which is why an offset can carry
// fractional seconds at all.
func parseHHMMSSFF(t []rune) (hour, minute, second, microsecond int, err error) {
	comps := [3]int{}
	hasSep := false
	pos := 0
	for comp := range comps {
		if comps[comp], err = fixedDigits(t, pos, 2); err != nil {
			return 0, 0, 0, 0, err
		}
		pos += 2
		if comp == 0 {
			hasSep = pos < len(t) && t[pos] == ':'
		}
		if pos >= len(t) || comp >= 2 {
			break
		}
		// Colons are all or nothing, the same way dashes are in the date. Note the
		// asymmetry CPython leaves in: a colon where none is expected is not
		// rejected here, it just fails to be a digit on the next pass round.
		if hasSep {
			if t[pos] != ':' {
				return 0, 0, 0, 0, ErrISOFormat
			}
			pos++
		}
	}
	if pos < len(t) {
		if t[pos] != '.' && t[pos] != ',' {
			return 0, 0, 0, 0, ErrISOFormat
		}
		pos++
		frac := t[pos:]
		if len(frac) == 0 {
			return 0, 0, 0, 0, ErrISOFormat
		}
		width := min(len(frac), 6)
		if microsecond, err = fixedDigits(frac, 0, width); err != nil {
			return 0, 0, 0, 0, err
		}
		// Every digit has to be a digit, including the ones being thrown away:
		// ".1234567x" is a ValueError, not 123456µs.
		for _, c := range frac[width:] {
			if !isASCIIDigit(c) {
				return 0, 0, 0, 0, ErrISOFormat
			}
		}
		if width < 6 {
			microsecond *= fractionScale[width-1]
		}
	}
	return comps[0], comps[1], comps[2], microsecond, nil
}

// fixedDigits reads exactly n ASCII digits at pos. Exactly: this is where the
// grammar's strictness lives, and it is why "2026-1-26" and "15:4:05" are
// ValueErrors rather than being read as short fields.
func fixedDigits(r []rune, pos, n int) (int, error) {
	if pos < 0 || pos+n > len(r) {
		return 0, ErrISOFormat
	}
	v := 0
	for _, c := range r[pos : pos+n] {
		if !isASCIIDigit(c) {
			return 0, ErrISOFormat
		}
		v = v*10 + int(c-'0')
	}
	return v, nil
}

func isASCIIDigit(r rune) bool { return r >= '0' && r <= '9' }

func indexRune(r []rune, target rune) int {
	for i, c := range r {
		if c == target {
			return i
		}
	}
	return -1
}

// isoWeekToGregorian is date.fromisocalendar: an ISO year, week and weekday to
// the calendar date they name. Week 53 exists only in an ISO year that has one --
// a year starting on a Thursday, or a leap year starting on a Wednesday -- so
// "2025-W53-1" is not a date while "2026-W53-1" is.
func isoWeekToGregorian(year, week, weekday int) (int, int, int, error) {
	if year < minYear || year > maxYear {
		return 0, 0, 0, ErrISOFormat
	}
	if week <= 0 || week >= 53 {
		longYear := false
		if week == 53 {
			firstWeekday := ymdToOrdinal(year, 1, 1) % 7
			longYear = firstWeekday == 4 || (firstWeekday == 3 && isLeap(year))
		}
		if !longYear {
			return 0, 0, 0, ErrISOFormat
		}
	}
	if weekday <= 0 || weekday >= 8 {
		return 0, 0, 0, ErrISOFormat
	}
	ordinal := isoWeek1Monday(year) + (week-1)*7 + (weekday - 1)
	y, m, d := ordinalToYMD(ordinal)
	return y, m, d, nil
}

// isoWeek1Monday is the ordinal of the Monday that starts ISO week 1 of year,
// which is the Monday of the week holding January 4th.
func isoWeek1Monday(year int) int {
	const thursday = 3
	firstDay := ymdToOrdinal(year, 1, 1)
	firstWeekday := (firstDay + 6) % 7
	week1Monday := firstDay - firstWeekday
	if firstWeekday > thursday {
		week1Monday += 7
	}
	return week1Monday
}

const (
	minYear = 1
	maxYear = 9999
)

// ordinalEpoch is ordinal 1: the proleptic Gregorian 0001-01-01, which is where
// Python counts days from. Go's calendar is proleptic Gregorian too, so the two
// agree on every date in range.
var ordinalEpoch = time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC)

// ymdToOrdinal counts days since 0001-01-01, which is ordinal 1.
//
// Arithmetic rather than a time.Time subtraction: a Duration is int64
// nanoseconds and saturates at about 292 years, so Sub over a range that reaches
// year 9999 would quietly return the same wrong answer for most of the calendar.
func ymdToOrdinal(year, month, day int) int {
	y := year - 1
	daysBeforeYear := y*365 + y/4 - y/100 + y/400
	// YearDay of the first of the month is one past the days before it, and it
	// knows about February without being told.
	daysBeforeMonth := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).YearDay() - 1
	return daysBeforeYear + daysBeforeMonth + day
}

// ordinalToYMD is the inverse. AddDate is exact and carries no Duration, so it is
// safe across the whole range -- and past it: an ISO week 53 in year 9999 lands
// in year 10000, which the caller's date validation is what rejects.
func ordinalToYMD(ordinal int) (int, int, int) {
	t := ordinalEpoch.AddDate(0, 0, ordinal-1)
	return t.Year(), int(t.Month()), t.Day()
}

func isLeap(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

func daysInMonth(year, month int) int {
	if month == 2 && isLeap(year) {
		return 29
	}
	return [...]int{31, 28, 31, 30, 31, 30, 31, 31, 30, 31, 30, 31}[month-1]
}

// validateDate is Python's date constructor: the range check that turns a
// well-formed string into a refusal. "2026-02-30" parses and is not a date.
func validateDate(year, month, day int) error {
	if year < minYear || year > maxYear || month < 1 || month > 12 {
		return ErrISOFormat
	}
	if day < 1 || day > daysInMonth(year, month) {
		return ErrISOFormat
	}
	return nil
}

// dayAfter is the hour-24 rollover. The date is already known valid, so this only
// has to carry.
func dayAfter(year, month, day int) (int, int, int) {
	if day < daysInMonth(year, month) {
		return year, month, day + 1
	}
	if month < 12 {
		return year, month + 1, 1
	}
	return year + 1, 1, 1
}
