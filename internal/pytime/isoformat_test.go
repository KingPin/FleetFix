package pytime

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// The want field is Python's datetime.isoformat() of the parsed value, or "" for
// a ValueError. Every expectation in this table was measured by feeding the input
// to CPython 3.14's datetime.fromisoformat, not derived from the grammar -- the
// grammar is what the implementation claims, and the point of the table is to
// disagree with it when it is wrong.
var isoFormatCases = []struct {
	name string
	in   string
	want string
}{
	// What docker and podman actually write: RFC3339Nano, nine fractional digits.
	{"docker started_at", "2026-07-26T15:04:05.123456789Z", "2026-07-26T15:04:05.123456+00:00"},
	{"docker started_at with explicit offset", "2026-07-26T15:04:05.123456789+00:00", "2026-07-26T15:04:05.123456+00:00"},
	{"docker zero value", "0001-01-01T00:00:00Z", "0001-01-01T00:00:00+00:00"},

	{"rfc3339 utc", "2026-07-26T15:04:05Z", "2026-07-26T15:04:05+00:00"},
	{"rfc3339 positive offset", "2026-07-26T15:04:05+05:30", "2026-07-26T15:04:05+05:30"},
	{"rfc3339 negative offset", "2026-07-26T15:04:05-05:00", "2026-07-26T15:04:05-05:00"},
	{"naive datetime", "2026-07-26T15:04:05", "2026-07-26T15:04:05"},
	{"date only", "2026-07-26", "2026-07-26T00:00:00"},

	// The separator is a slot, not a character: whatever occupies it is consumed.
	{"space separator", "2026-07-26 15:04:05", "2026-07-26T15:04:05"},
	{"hash separator", "2026-07-26#15:04:05", "2026-07-26T15:04:05"},
	{"multibyte separator", "2026-07-26é15:04:05", "2026-07-26T15:04:05"},
	{"astral separator", "2026-07-26\U0001f60015:04:05", "2026-07-26T15:04:05"},
	{"lowercase t separator", "2026-07-26t15:04:05", "2026-07-26T15:04:05"},

	{"basic date", "20260726", "2026-07-26T00:00:00"},
	{"basic date and time", "20260726T150405", "2026-07-26T15:04:05"},
	{"basic with fraction", "20260726T150405.5", "2026-07-26T15:04:05.500000"},
	{"basic with offset", "20260726T150405+0530", "2026-07-26T15:04:05+05:30"},
	{"extended date basic time", "2026-07-26T150405Z", "2026-07-26T15:04:05+00:00"},

	{"week date", "2026-W30-7", "2026-07-26T00:00:00"},
	{"week date without day", "2026-W30", "2026-07-20T00:00:00"},
	{"basic week date", "2026W307", "2026-07-26T00:00:00"},
	{"basic week date without day", "2026W30", "2026-07-20T00:00:00"},
	{"week date with time", "2026-W30-7T15:04:05", "2026-07-26T15:04:05"},
	{"week 53 of a long year", "2026-W53-1", "2026-12-28T00:00:00"},
	{"week 53 of a leap long year", "2020-W53-1", "2020-12-28T00:00:00"},
	{"week 53 of an ordinary year", "2025-W53-1", ""},
	{"week 1 reaches back into last year", "2026-W01-1", "2025-12-29T00:00:00"},
	{"earliest week date", "0001-W01-1", "0001-01-01T00:00:00"},
	{"last week date in range", "9999-W52-5", "9999-12-31T00:00:00"},
	{"week date past the last year", "9999-W52-7", ""},
	{"week 53 of the last year", "9999-W53-1", ""},

	{"hour only", "2026-07-26T15", "2026-07-26T15:00:00"},
	{"hour and minute", "2026-07-26T15:04", "2026-07-26T15:04:00"},
	{"comma fraction", "2026-07-26T15:04:05,5", "2026-07-26T15:04:05.500000"},

	{"1 fractional digit", "2026-07-26T15:04:05.1", "2026-07-26T15:04:05.100000"},
	{"2 fractional digits", "2026-07-26T15:04:05.12", "2026-07-26T15:04:05.120000"},
	{"3 fractional digits", "2026-07-26T15:04:05.123", "2026-07-26T15:04:05.123000"},
	{"4 fractional digits", "2026-07-26T15:04:05.1234", "2026-07-26T15:04:05.123400"},
	{"5 fractional digits", "2026-07-26T15:04:05.12345", "2026-07-26T15:04:05.123450"},
	{"6 fractional digits", "2026-07-26T15:04:05.123456", "2026-07-26T15:04:05.123456"},
	// Truncated, not rounded: the 7th digit is a 9 in the second case and the
	// microseconds still end in 6.
	{"7 fractional digits truncate", "2026-07-26T15:04:05.1234567", "2026-07-26T15:04:05.123456"},
	{"7 fractional digits do not round", "2026-07-26T15:04:05.1234569", "2026-07-26T15:04:05.123456"},
	{"explicit zero fraction", "2026-07-26T15:04:05.000000", "2026-07-26T15:04:05"},

	{"hour-only offset", "2026-07-26T15:04:05+05", "2026-07-26T15:04:05+05:00"},
	{"basic offset", "2026-07-26T15:04:05+0530", "2026-07-26T15:04:05+05:30"},
	{"offset with seconds", "2026-07-26T15:04:05+05:30:45", "2026-07-26T15:04:05+05:30:45"},
	{"offset with microseconds", "2026-07-26T15:04:05+05:30:45.123456", "2026-07-26T15:04:05+05:30:45.123456"},
	{"basic offset with seconds", "2026-07-26T15:04:05+053045", "2026-07-26T15:04:05+05:30:45"},
	{"negative zero offset is utc", "2026-07-26T15:04:05-00:00", "2026-07-26T15:04:05+00:00"},
	{"positive zero offset", "2026-07-26T15:04:05+00:00", "2026-07-26T15:04:05+00:00"},
	// The components are not range-checked, only the total: 60 minutes is an hour.
	{"offset minute 60 carries", "2026-07-26T15:04:05+05:60", "2026-07-26T15:04:05+06:00"},
	{"largest offset", "2026-07-26T15:04:05+23:59:59.999999", "2026-07-26T15:04:05+23:59:59.999999"},
	{"offset of exactly a day", "2026-07-26T15:04:05+23:60", ""},
	{"offset over a day", "2026-07-26T15:04:05+24:00", ""},
	{"largest negative whole-minute offset", "2026-07-26T15:04:05-23:59", "2026-07-26T15:04:05-23:59"},

	{"hour 24 is tomorrow", "2026-07-26T24:00", "2026-07-27T00:00:00"},
	{"hour 24 alone", "2026-07-26T24", "2026-07-27T00:00:00"},
	{"hour 24 with seconds", "2026-07-26T24:00:00", "2026-07-27T00:00:00"},
	{"hour 24 with zero fraction", "2026-07-26T24:00:00.000000", "2026-07-27T00:00:00"},
	{"hour 24 aware", "2026-07-26T24:00:00Z", "2026-07-27T00:00:00+00:00"},
	{"hour 24 with a second", "2026-07-26T24:00:01", ""},
	{"hour 24 with a microsecond", "2026-07-26T24:00:00.000001", ""},
	{"hour 24 carries the month", "2026-01-31T24:00", "2026-02-01T00:00:00"},
	{"hour 24 carries the year", "2026-12-31T24:00", "2027-01-01T00:00:00"},
	{"hour 24 carries into a leap day", "2024-02-28T24:00", "2024-02-29T00:00:00"},
	{"hour 24 past the last day", "9999-12-31T24:00", ""},
	// The date is validated before the rollover, so this is a bad month and not
	// the day after one.
	{"hour 24 on an impossible date", "2026-13-01T24:00", ""},

	{"leap day", "2024-02-29", "2024-02-29T00:00:00"},
	{"non-leap 29 february", "2026-02-29", ""},
	{"400-year leap day", "2000-02-29", "2000-02-29T00:00:00"},
	{"century non-leap day", "1900-02-29", ""},
	{"30 february", "2026-02-30", ""},
	{"month zero", "2026-00-01", ""},
	{"day zero", "2026-01-00", ""},
	{"day 32", "2026-01-32", ""},
	{"month 13", "2026-13-01", ""},
	{"year zero", "0000-01-01", ""},
	{"last representable date", "9999-12-31", "9999-12-31T00:00:00"},

	// Fields are exactly as wide as the grammar says, which is where most
	// human-written input falls over.
	{"one-digit month", "2026-1-26", ""},
	{"one-digit minute", "2026-07-26T15:4:05", ""},
	{"half-extended date", "2026-0726", ""},
	{"half-extended date the other way", "202607-26", ""},
	{"extended week date without the second dash", "2026-W307", ""},
	{"basic week date with a dash", "2026W30-7", ""},
	{"leading space", " 2026-07-26", ""},
	{"trailing space", "2026-07-26 ", ""},
	{"lowercase z", "2026-07-26T15:04:05z", ""},
	{"non-ascii digits", "٢٠٢٦-٠٧-٢٦", ""},
	{"empty", "", ""},
	{"too short to be a date", "2026-0", ""},
	{"separator with no time", "2026-07-26T", ""},
	{"trailing colon after the hour", "2026-07-26T15:", ""},
	{"trailing colon after the minute", "2026-07-26T15:04:", ""},
	{"fraction marker with no digits", "2026-07-26T15:04:05.", ""},
	{"offset marker with no offset", "2026-07-26T15:04:05+", ""},
	{"one-digit offset", "2026-07-26T15:04:05+0", ""},
	{"three-digit offset", "2026-07-26T15:04:05+050", ""},
	{"offset seconds one digit short", "2026-07-26T15:04:05+05:30:4", ""},
	{"trailing junk", "2026-07-26T15:04:05x", ""},
	{"an extra second digit", "2026-07-26T15:04:055", ""},
	// The offset marker is the first '-' anywhere, so this splits at the dash and
	// leaves "+05" as the clock rather than reading as a +05 offset.
	{"mixed offset punctuation", "2026-07-26T15:04:05+05-30", ""},
	{"z followed by an offset", "2026-07-26T15:04:05Z+05:30", ""},
	{"junk among the discarded fraction digits", "2026-07-26T15:04:05.1234567x", ""},
	{"digit in the week-date separator slot", "2026-W30-77", ""},

	// Where the separator lands is inferred from the string's length, so these
	// pick at that inference rather than at the fields. The first one is the
	// alarming one: it is a week date, a separator that happens to be a digit,
	// and an hour, and it means 23:00 on the Monday of week 30.
	{"basic week date, digit separator, hour", "2026W30123", "2026-07-20T23:00:00"},
	{"basic week date with an odd digit run", "2026W3012:0", ""},
	{"basic week date one digit too long", "2026W3012", ""},
	{"nine characters of extended week date", "2026-W30-", ""},
	{"non-digit week number", "2026-Wab", ""},
	{"non-digit weekday", "2026-W30-x", ""},
	{"weekday zero", "2026-W30-0", ""},
	{"weekday 8", "2026-W30-8", ""},
	{"week date in year zero", "0000-W01-1", ""},
	{"non-digit month", "2026-ab-26", ""},
	{"non-digit day", "2026-07-ab", ""},
	{"colon after the hour but not the minute", "2026-07-26T15:0405", ""},
	{"non-digit fraction", "2026-07-26T15:04:05.x", ""},
}

func TestFromISOFormat(t *testing.T) {
	t.Parallel()

	for _, tc := range isoFormatCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := FromISOFormat(tc.in)
			if tc.want == "" {
				if err == nil {
					t.Fatalf("FromISOFormat(%q) = %s, want ErrISOFormat", tc.in, got.ISOFormat())
				}
				if !errors.Is(err, ErrISOFormat) {
					t.Fatalf("FromISOFormat(%q) returned %v, want ErrISOFormat", tc.in, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromISOFormat(%q) = %v, want %s", tc.in, err, tc.want)
			}
			if s := got.ISOFormat(); s != tc.want {
				t.Errorf("FromISOFormat(%q).ISOFormat() = %s, want %s", tc.in, s, tc.want)
			}
		})
	}
}

func TestFromISOFormatFields(t *testing.T) {
	t.Parallel()

	// ISOFormat is a complete encoding of a Time, so the table above covers every
	// field -- but only through one function. These spot-check the struct directly
	// so a matched pair of bugs in the parser and the formatter cannot hide.
	cases := []struct {
		in   string
		want Time
	}{
		{"2026-07-26", Time{Year: 2026, Month: 7, Day: 26}},
		{
			"2026-07-26T15:04:05.123456789Z",
			Time{Year: 2026, Month: 7, Day: 26, Hour: 15, Minute: 4, Second: 5, Microsecond: 123456, Aware: true},
		},
		{
			"2026-07-26T15:04:05-05:30:45.000001",
			Time{
				Year: 2026, Month: 7, Day: 26, Hour: 15, Minute: 4, Second: 5,
				Offset: -(5*time.Hour + 30*time.Minute + 45*time.Second + time.Microsecond),
				Aware:  true,
			},
		},
	}
	for _, tc := range cases {
		got, err := FromISOFormat(tc.in)
		if err != nil {
			t.Fatalf("FromISOFormat(%q) = %v", tc.in, err)
		}
		if got != tc.want {
			t.Errorf("FromISOFormat(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestWallIsTheReadingAsWritten(t *testing.T) {
	t.Parallel()

	// An offset does not move the wall clock. Both of these name 15:04:05 on the
	// wall; only Instant tells them apart.
	for _, in := range []string{"2026-07-26T15:04:05", "2026-07-26T15:04:05+05:30"} {
		got, err := FromISOFormat(in)
		if err != nil {
			t.Fatalf("FromISOFormat(%q) = %v", in, err)
		}
		want := time.Date(2026, time.July, 26, 15, 4, 5, 0, time.UTC)
		if w := got.Wall(); !w.Equal(want) {
			t.Errorf("FromISOFormat(%q).Wall() = %s, want %s", in, w, want)
		}
	}
}

func TestInstantHasNoAnswerForANaiveReading(t *testing.T) {
	t.Parallel()

	// This is the property that keeps v1's is_restart_loop TypeError visible
	// instead of silently answering as though the reading were UTC.
	naive, err := FromISOFormat("2026-07-26T15:04:05")
	if err != nil {
		t.Fatalf("FromISOFormat: %v", err)
	}
	if got, ok := naive.Instant(); ok {
		t.Errorf("Instant() on a naive reading returned %s, want ok=false", got)
	}

	aware, err := FromISOFormat("2026-07-26T15:04:05+05:30")
	if err != nil {
		t.Fatalf("FromISOFormat: %v", err)
	}
	got, ok := aware.Instant()
	if !ok {
		t.Fatal("Instant() on an aware reading returned ok=false")
	}
	if want := time.Date(2026, time.July, 26, 9, 34, 5, 0, time.UTC); !got.Equal(want) {
		t.Errorf("Instant() = %s, want %s", got, want)
	}
}

func TestISOFormat(t *testing.T) {
	t.Parallel()

	// The offset is spelled to the finest unit it uses, which is Python's rule and
	// the reason these are asserted from a struct rather than round-tripped: a
	// parse-then-format test cannot show that "+00:00" is not "+00:00:00".
	base := Time{Year: 2026, Month: 7, Day: 26, Hour: 15, Minute: 4, Second: 5}
	cases := []struct {
		name string
		in   Time
		want string
	}{
		{"naive", base, "2026-07-26T15:04:05"},
		{"utc", withOffset(base, 0), "2026-07-26T15:04:05+00:00"},
		{"whole hours", withOffset(base, 5*time.Hour), "2026-07-26T15:04:05+05:00"},
		{"hours and minutes", withOffset(base, 5*time.Hour+30*time.Minute), "2026-07-26T15:04:05+05:30"},
		{"with offset seconds", withOffset(base, 5*time.Hour+30*time.Minute+45*time.Second), "2026-07-26T15:04:05+05:30:45"},
		{
			"with offset microseconds",
			withOffset(base, 5*time.Hour+30*time.Minute+45*time.Second+123456*time.Microsecond),
			"2026-07-26T15:04:05+05:30:45.123456",
		},
		{
			"offset microseconds with zero seconds still print the seconds",
			withOffset(base, 5*time.Hour+time.Microsecond),
			"2026-07-26T15:04:05+05:00:00.000001",
		},
		{"negative", withOffset(base, -(5*time.Hour + 30*time.Minute)), "2026-07-26T15:04:05-05:30"},
		{"microseconds", Time{Year: 1, Month: 1, Day: 1, Microsecond: 1}, "0001-01-01T00:00:00.000001"},
		{"last representable", Time{Year: 9999, Month: 12, Day: 31, Hour: 23, Minute: 59, Second: 59, Microsecond: 999999}, "9999-12-31T23:59:59.999999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.in.ISOFormat(); got != tc.want {
				t.Errorf("ISOFormat() = %s, want %s", got, tc.want)
			}
		})
	}
}

func withOffset(t Time, off time.Duration) Time {
	t.Offset, t.Aware = off, true
	return t
}

func TestFromISOFormatDepartsFromTheCAcceleratorWhereItDisagreesWithCPythonsOwnReference(t *testing.T) {
	t.Parallel()

	// Each of these is accepted by the C accelerator that a v1 install actually
	// runs, rejected by the reference implementation in _pydatetime.py, and
	// rejected here -- see FromISOFormat's doc comment for why the intersection of
	// the two is the boundary drawn. The answer the accelerator gives is recorded
	// alongside so the departure is legible rather than merely asserted; every one
	// of them reads a malformed string as a shorter valid one.
	cases := []struct {
		in          string
		accelerator string
	}{
		{"2026-07-26T15:04:05 Z", "2026-07-26T15:04:05+00:00"},
		{"2026-07-26T15:04:05,+05:30:45.123456", "2026-07-26T15:04:05+05:30:45.123456"},
		{"2026-07-26T15:04:005Z", "2026-07-26T15:04:00+00:00"},
		{"2026-07-26T15:+04:05", "2026-07-26T15:00:00+04:05"},
		{"2026-07-26T15:04:+05", "2026-07-26T15:04:00+05:00"},
		{"0001-01-01T00:-00:00", "0001-01-01T00:00:00+00:00"},
	}
	for _, tc := range cases {
		got, err := FromISOFormat(tc.in)
		if err == nil {
			t.Errorf("FromISOFormat(%q) = %s; want ErrISOFormat (the accelerator says %s, its own reference says ValueError)",
				tc.in, got.ISOFormat(), tc.accelerator)
		}
	}
}

func TestEveryDepartureIsUnreachableFromContainerRuntimeOutput(t *testing.T) {
	t.Parallel()

	// The departures above only matter if something can produce one. Docker and
	// podman both render .State.StartedAt with Go's own RFC3339Nano, so the guard
	// is that every shape that formatter emits parses to the same instant. Nanos
	// below microsecond precision are the one loss, and it is Python's loss too.
	for _, in := range []time.Time{
		time.Date(1, time.January, 1, 0, 0, 0, 0, time.UTC),
		time.Date(2026, time.July, 26, 15, 4, 5, 0, time.UTC),
		time.Date(2026, time.July, 26, 15, 4, 5, 123456789, time.UTC),
		time.Date(2026, time.July, 26, 15, 4, 5, 100000000, time.UTC),
		time.Date(2026, time.July, 26, 15, 4, 5, 1, time.UTC),
		time.Date(2026, time.July, 26, 15, 4, 5, 0, time.FixedZone("", 5*3600+1800)),
		time.Date(2026, time.July, 26, 15, 4, 5, 999999999, time.FixedZone("", -5*3600)),
		time.Date(9999, time.December, 31, 23, 59, 59, 999999999, time.UTC),
	} {
		s := in.Format(time.RFC3339Nano)
		got, err := FromISOFormat(s)
		if err != nil {
			t.Errorf("FromISOFormat(%q) = %v, want the instant back", s, err)
			continue
		}
		instant, ok := got.Instant()
		if !ok {
			t.Errorf("FromISOFormat(%q) came back naive", s)
			continue
		}
		if want := in.Truncate(0).Add(-time.Duration(in.Nanosecond() % 1000)); !instant.Equal(want) {
			t.Errorf("FromISOFormat(%q).Instant() = %s, want %s", s, instant, want)
		}
	}
}

func TestParseDateDoesNotTrustItsCaller(t *testing.T) {
	t.Parallel()

	// Two of parseDate's checks cannot be reached through FromISOFormat, and that
	// is worth stating rather than leaving as a gap in the coverage report. The
	// separator search only ever returns 7, 8 or 10, and it only returns 10 for a
	// week date when there is a dash at index 8 -- so a date slice of another
	// length, or an extended week date missing its second dash, arrives only from
	// a future caller or a bug in that search. These are the checks that would
	// catch one, so they are exercised directly.
	for _, d := range []string{
		"2026-07-2",   // nine characters: not a shape the search can produce
		"2026-07-261", // eleven
		"2026-W3012",  // ten characters of week date with no dash before the day
	} {
		if _, _, _, err := parseDate([]rune(d)); err == nil {
			t.Errorf("parseDate(%q) returned a date; want ErrISOFormat", d)
		}
		// And the same string through the front door, which must also refuse --
		// by a different route, but the answer the caller sees is the same.
		if _, err := FromISOFormat(d); err == nil {
			t.Errorf("FromISOFormat(%q) returned a date; want ErrISOFormat", d)
		}
	}
}

func TestOrdinalRoundTripsAcrossTheWholeYearRange(t *testing.T) {
	t.Parallel()

	// The guard on the Duration saturation trap: a time.Time subtraction would
	// return the same wrong answer for most of this range, and the only symptom
	// would be week dates quietly landing on the wrong day. Ordinals have to be
	// consecutive and invertible from year 1 to year 9999, so both ends and every
	// month boundary in between are checked.
	want := 1
	for year := minYear; year <= maxYear; year++ {
		for month := 1; month <= 12; month++ {
			if got := ymdToOrdinal(year, month, 1); got != want {
				t.Fatalf("ymdToOrdinal(%04d-%02d-01) = %d, want %d", year, month, got, want)
			}
			y, m, d := ordinalToYMD(want)
			if y != year || m != month || d != 1 {
				t.Fatalf("ordinalToYMD(%d) = %04d-%02d-%02d, want %04d-%02d-01", want, y, m, d, year, month)
			}
			want += daysInMonth(year, month)
		}
	}
	// date.max.toordinal() is 3652059, measured from the interpreter.
	if got := ymdToOrdinal(maxYear, 12, 31); got != 3652059 {
		t.Errorf("ymdToOrdinal(9999-12-31) = %d, want 3652059", got)
	}
}

func TestWeekDatesInvertGosOwnISOWeek(t *testing.T) {
	t.Parallel()

	// An independent check on the week-date arithmetic: Go's time.ISOWeek is a
	// separate implementation of the same calendar, so if the two agree on every
	// day of a fifty-year span the port is not merely self-consistent.
	end := time.Date(2050, time.January, 1, 0, 0, 0, 0, time.UTC)
	for day := time.Date(2000, time.January, 1, 0, 0, 0, 0, time.UTC); day.Before(end); day = day.AddDate(0, 0, 1) {
		isoYear, isoWeek := day.ISOWeek()
		weekday := int(day.Weekday())
		if weekday == 0 {
			weekday = 7 // Go counts Sunday as 0; ISO counts it as 7.
		}
		y, m, d, err := isoWeekToGregorian(isoYear, isoWeek, weekday)
		if err != nil {
			t.Fatalf("isoWeekToGregorian(%d, %d, %d) = %v, for %s", isoYear, isoWeek, weekday, err, day.Format(time.DateOnly))
		}
		if y != day.Year() || m != int(day.Month()) || d != day.Day() {
			t.Fatalf("isoWeekToGregorian(%d, %d, %d) = %04d-%02d-%02d, want %s",
				isoYear, isoWeek, weekday, y, m, d, day.Format(time.DateOnly))
		}
	}
}

func FuzzFromISOFormat(f *testing.F) {
	for _, tc := range isoFormatCases {
		f.Add(tc.in)
	}
	f.Fuzz(func(t *testing.T, s string) {
		got, err := FromISOFormat(s)
		if err != nil {
			if !errors.Is(err, ErrISOFormat) {
				t.Fatalf("FromISOFormat(%q) returned %v, want ErrISOFormat", s, err)
			}
			return
		}
		// Anything that parsed is a real date in Python's range, with a real clock
		// and an offset under a day. A parser bug that skipped a range check would
		// otherwise surface much later as a time.Date silently normalising.
		if err := validateDate(got.Year, got.Month, got.Day); err != nil {
			t.Fatalf("FromISOFormat(%q) = %+v, which is not a date", s, got)
		}
		if got.Hour > 23 || got.Minute > 59 || got.Second > 59 || got.Microsecond > 999999 {
			t.Fatalf("FromISOFormat(%q) = %+v, which is not a clock", s, got)
		}
		if got.Offset <= -24*time.Hour || got.Offset >= 24*time.Hour {
			t.Fatalf("FromISOFormat(%q) has offset %s, which is not under a day", s, got.Offset)
		}
		if !got.Aware && got.Offset != 0 {
			t.Fatalf("FromISOFormat(%q) = %+v: a naive reading carries an offset", s, got)
		}
		// The wire form has to be accepted by the parser that produced it, and land
		// in the same place. This is what makes the differential comparison mean
		// something: the oracle compares isoformat() strings, so a Time that cannot
		// survive its own round trip is a divergence waiting for the right input.
		wire := got.ISOFormat()
		again, err := FromISOFormat(wire)
		if err != nil {
			t.Fatalf("FromISOFormat(%q) rendered %q, which does not parse: %v", s, wire, err)
		}
		if again != got {
			t.Fatalf("FromISOFormat(%q) = %+v, but its wire form %q reads back as %+v", s, got, wire, again)
		}
		if strings.ContainsAny(wire, "\x00") {
			t.Fatalf("FromISOFormat(%q) rendered %q, which is not a datetime", s, wire)
		}
	})
}
