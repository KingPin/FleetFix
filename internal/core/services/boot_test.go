package services

import (
	"regexp"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
	"github.com/google/go-cmp/cmp"
)

// Expectations come from src/fleetfix/modules/services/boot.py's parse_blame
// under CPython 3.14.6, fed the same literals, except where a case is marked as
// a departure.

func TestParseBlameFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    []BlameEntry
	}{
		{"systemd_analyze/blame_blank_lines.txt", []BlameEntry{
			{Unit: "foo.service", DurationMS: 5000},
		}},
		{"systemd_analyze/blame_classic.txt", []BlameEntry{
			{Unit: "archlinux-keyring-wkd-sync.service", DurationMS: 59647},
			{Unit: "NetworkManager-wait-online.service", DurationMS: 5569},
			{Unit: "NetworkManager.service", DurationMS: 559},
			{Unit: "long-thing.service", DurationMS: 62234},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got := ParseBlame(fixture.Text(t, tt.fixture))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseBlame(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

func TestParseBlame(t *testing.T) {
	// one is the single-entry answer most cases below expect.
	one := func(unit string, ms int64) []BlameEntry {
		return []BlameEntry{{Unit: unit, DurationMS: ms}}
	}

	tests := []struct {
		name string
		in   string
		want []BlameEntry
	}{
		{"empty", "", []BlameEntry{}},

		// The shapes systemd-analyze actually prints.
		{"seconds", "59.647s foo.service\n", one("foo.service", 59647)},
		{"seconds indented", "     5.569s bar.service\n", one("bar.service", 5569)},
		{"milliseconds", "      559ms baz.service\n", one("baz.service", 559)},
		{"minutes and seconds", "1min 2.234s long.service\n", one("long.service", 62234)},
		{"minutes alone", "1min x.service\n", one("x.service", 60000)},
		{"zero seconds", "0s x.service\n", one("x.service", 0)},
		{"zero milliseconds", "0ms x.service\n", one("x.service", 0)},
		{"two entries", "59.647s a.service\n559ms b.service\n", []BlameEntry{
			{Unit: "a.service", DurationMS: 59647},
			{Unit: "b.service", DurationMS: 559},
		}},

		// A line with no whitespace cannot be split into a duration and a unit. v1
		// unpacks a two-element rsplit, so this raised and the line was skipped.
		{"one token", "x.service\n", []BlameEntry{}},
		{"no duration", "foo bar\n", []BlameEntry{}},
		// A number with no suffix is not a duration.
		{"bare number", "5 foo\n", []BlameEntry{}},
		{"whitespace only line", "   \n", []BlameEntry{}},
		{"blank lines", "\n\n5s foo\n\n", one("foo", 5000)},
		{"no trailing newline", "5s foo", one("foo", 5000)},
		{"crlf", "5s foo\r\n", one("foo", 5000)},

		// The unit is the last whitespace-separated field, whatever the whitespace
		// is and however much of it there is.
		{"tab before the unit", "5s\tfoo\n", one("foo", 5000)},
		{"run of spaces before the unit", "5s  foo bar\n", one("bar", 5000)},
		{"non-breaking space before the unit", "5s" + nbsp + "foo\n", one("foo", 5000)},
		{"three tokens", "5s a b\n", one("b", 5000)},

		// The seconds \b: an s followed by a word character is not a seconds
		// suffix, and one followed by anything else is.
		{"s followed by a letter", "5sx foo\n", []BlameEntry{}},
		{"s followed by a dot", "5s. foo\n", one("foo", 5000)},
		{"ms followed by a letter", "5msx foo\n", []BlameEntry{}},

		// Each search takes its first match independently, which is what makes
		// these two read the way they do. systemd prints neither shape.
		{"min glued to seconds", "1min30s foo\n", one("foo", 90000)},
		{"two seconds tokens", "5s6s foo\n", one("foo", 6000)},
		{"seconds and milliseconds", "12.5s 3.5ms foo\n", one("foo", 12503)},
		// "min" contains no ms, so the milliseconds search does not fire here.
		{"minutes only, not milliseconds", "1min foo\n", one("foo", 60000)},

		// int(float(...)) truncates towards zero rather than rounding.
		{"fractional milliseconds truncate", "1.9ms foo\n", one("foo", 1)},
		{"leading dot", ".5s foo\n", one("foo", 500)},

		// Python's \d on str is Nd, and float()/int() accept those digits, so an
		// Arabic-Indic five really is five seconds.
		{"unicode digit seconds", "\u0665s foo\n", one("foo", 5000)},
		{"unicode digit milliseconds", "\u0665ms foo\n", one("foo", 5)},

		// The suffixes are case-sensitive in v1, so an upper-cased one is no
		// duration at all.
		{"uppercase s", "5S foo\n", []BlameEntry{}},
		{"uppercase min", "1MIN foo\n", []BlameEntry{}},

		// An underscore is not in [\d.], so it breaks the run: the match is the
		// "0" after it, and the answer is zero milliseconds rather than ten
		// seconds. v1 agrees, having been asked.
		{"underscore inside the number", "1_0s foo\n", one("foo", 0)},
		// A sign is not in [\d.] either, so the minus is simply not part of the
		// number and five seconds is what both implementations report.
		{"leading minus", "-5s foo\n", one("foo", 5000)},

		// A suffix with no digits in front of it matches nothing.
		{"s with nothing before it", "s foo\n", []BlameEntry{}},
		{"min with nothing before it", "min foo\n", []BlameEntry{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseBlame(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseBlame(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// TestParseBlameDepartsOnInputsV1CannotAnswer covers the two failure classes
// where Go deliberately does not reproduce v1, both of them named in parseTime's
// comment.
//
// The first class is a capture that matches [\d.]+ without being a number: v1's
// float() raises ValueError and the exception escapes parse_blame, so v1's answer
// to such a line is a traceback rather than a value. The second is a duration
// that does not fit in an int64, where v1's arbitrary-precision ints report the
// real number.
//
// Both drop the line. Neither is reachable from any fixture in the corpus, which
// is why the differential needs no known_divergences.yaml entry for them.
func TestParseBlameDepartsOnInputsV1CannotAnswer(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		// v1: ValueError out of float().
		{"two dots in the seconds", "1.2.3s foo\n"},
		{"a lone dot as the seconds", ".s foo\n"},
		{"two dots in the milliseconds", "1.2.3ms foo\n"},
		{"a lone dot as the milliseconds", ".ms foo\n"},

		// v1: a real, enormous number. Sixteen digits of seconds is 300 million
		// years of boot, and systemd cannot print it.
		{"seconds past int64", "9999999999999999s foo\n"},
		{"milliseconds past int64", "99999999999999999999ms foo\n"},
		{"minutes past int64", strings.Repeat("9", 20) + "min foo\n"},
		{"minutes whose product overflows", "999999999999999min foo\n"},
		{"minutes plus seconds overflow the sum", "100000000000000min 3300000000000000s foo\n"},
		{"seconds plus milliseconds overflow the sum", "9000000000000000s 300000000000000000ms foo\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseBlame(tt.in); len(got) != 0 {
				t.Errorf("ParseBlame(%q) = %#v, want the line dropped", tt.in, got)
			}
		})
	}
}

// TestParseBlameOverflowInputsAreReallyTheOverflowPath guards the cases above
// against passing for the wrong reason. Each names a number the pattern must
// still capture, so a typo that stops the regex matching at all would leave the
// departure tests green while testing nothing.
func TestParseBlameOverflowInputsAreReallyTheOverflowPath(t *testing.T) {
	tests := []struct {
		token string
		re    string
		want  string
	}{
		{"9999999999999999s", "seconds", "9999999999999999"},
		{"99999999999999999999ms", "milliseconds", "99999999999999999999"},
		{strings.Repeat("9", 20) + "min", "minutes", strings.Repeat("9", 20)},
		{"999999999999999min", "minutes", "999999999999999"},
		{"100000000000000min 3300000000000000s", "seconds", "3300000000000000"},
		{"9000000000000000s 300000000000000000ms", "milliseconds", "300000000000000000"},
	}
	res := map[string]*regexp.Regexp{
		"minutes":      reMinutes,
		"seconds":      reSeconds,
		"milliseconds": reMilliseconds,
	}
	for _, tt := range tests {
		t.Run(tt.token+" "+tt.re, func(t *testing.T) {
			m := res[tt.re].FindStringSubmatch(tt.token)
			if m == nil {
				t.Fatalf("the %s pattern did not match %q at all", tt.re, tt.token)
			}
			if m[1] != tt.want {
				t.Errorf("the %s pattern captured %q from %q, want %q", tt.re, m[1], tt.token, tt.want)
			}
		})
	}
}

// TestRsplitOnce pins the Python rsplit(None, 1) this port hand-rolls, because
// pytext has no equivalent and the whole-run behaviour is what keeps "5s  foo"
// from reporting a duration of "5s ".
func TestRsplitOnce(t *testing.T) {
	tests := []struct {
		in    string
		left  string
		right string
		ok    bool
	}{
		{"1min  2.234s foo", "1min  2.234s", "foo", true},
		{"5s foo", "5s", "foo", true},
		{"5s\t\tfoo", "5s", "foo", true},
		{"5s" + nbsp + "foo", "5s", "foo", true},
		{"foo", "", "", false},
		{"", "", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			left, right, ok := rsplitOnce(tt.in)
			if left != tt.left || right != tt.right || ok != tt.ok {
				t.Errorf("rsplitOnce(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tt.in, left, right, ok, tt.left, tt.right, tt.ok)
			}
		})
	}
}

func TestParseBlameReturnsEmptyNotNil(t *testing.T) {
	if got := ParseBlame(""); got == nil {
		t.Error(`ParseBlame("") = nil, want an empty slice`)
	}
}

func FuzzParseBlame(f *testing.F) {
	for _, seed := range []string{
		"",
		"59.647s a.service\n5.569s b.service\n559ms c.service\n",
		"1min 2.234s long.service\n",
		"1min30s foo\n5s6s bar\n",
		".s foo\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, e := range ParseBlame(in) {
			// The unit is the last field of a whitespace split, so it can be
			// neither empty nor whitespace-bearing.
			if e.Unit == "" {
				t.Fatalf("ParseBlame(%q) produced an entry with no unit: %#v", in, e)
			}
			if strings.IndexFunc(e.Unit, pytext.IsSpace) >= 0 {
				t.Fatalf("ParseBlame(%q) produced unit %q, which holds whitespace", in, e.Unit)
			}
			// Neither pattern can capture a sign, and the sum is checked, so a
			// negative boot time is not a reading either implementation can
			// produce.
			if e.DurationMS < 0 {
				t.Fatalf("ParseBlame(%q) produced a negative duration: %#v", in, e)
			}
		}
	})
}
