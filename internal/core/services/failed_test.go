package services

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
	"github.com/google/go-cmp/cmp"
)

// Expectations come from src/fleetfix/modules/services/failed.py's
// parse_failed_units and parse_show_user under CPython 3.14.6, fed the same
// literals.

// nbsp is U+00A0. It is spelled out because it is whitespace to Python and not
// to a reader looking at the source, and several cases below turn on which of
// those two facts applies.
const nbsp = "\u00a0"

// unit is one failed unit with the four fixed columns every case shares, so the
// tables below only have to say what differs.
func unit(name, description string) FailedUnit {
	return FailedUnit{Name: name, Load: "loaded", Active: "failed", Sub: "failed", Description: description}
}

func TestParseFailedUnitsFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    []FailedUnit
	}{
		{"systemctl/failed_multi_word_description.txt", []FailedUnit{
			unit("kafka.service", "Apache Kafka brokers and topics"),
		}},
		{"systemctl/failed_short_row.txt", []FailedUnit{}},
		{"systemctl/failed_three_units.txt", []FailedUnit{
			unit("alpha.service", "Alpha Service"),
			unit("beta.service", "Beta Service"),
			unit("gamma.service", "Gamma Service"),
		}},
		{"systemctl/failed_two_units.txt", []FailedUnit{
			unit("myapp.service", "My Application Service"),
			unit("other.service", "Other Daemon"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got := ParseFailedUnits(fixture.Text(t, tt.fixture))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseFailedUnits(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

func TestParseFailedUnits(t *testing.T) {
	const row = "a.service loaded failed failed Thing\n"

	tests := []struct {
		name string
		in   string
		want []FailedUnit
	}{
		{"empty", "", []FailedUnit{}},
		{"header", "UNIT LOAD ACTIVE SUB DESCRIPTION\n", []FailedUnit{}},
		// The skip prefix is "UNIT " with a trailing space, so a header systemd
		// separated with tabs is read as a unit named UNIT. --no-legend means this
		// header should not be printed at all, which is why v1 never noticed.
		{"header separated by tabs", "UNIT\tLOAD\tACTIVE\tSUB\tDESCRIPTION\n", []FailedUnit{
			{Name: "UNIT", Load: "LOAD", Active: "ACTIVE", Sub: "SUB", Description: "DESCRIPTION"},
		}},
		{"header word alone", "UNIT\n", []FailedUnit{}},
		// The summary line systemd prints after the table is indented, and this is
		// the prefix that drops it. It is two ASCII spaces exactly, so nothing else
		// that looks like an indent is skipped.
		{"two space indent", "  " + row, []FailedUnit{}},
		{"one space indent", " " + row, []FailedUnit{unit("a.service", "Thing")}},
		{"tab indent", "\t" + row, []FailedUnit{unit("a.service", "Thing")}},
		{"non-breaking space indent", nbsp + nbsp + row, []FailedUnit{unit("a.service", "Thing")}},

		// Four columns are the minimum; three is a truncated line.
		{"three fields", "a.service loaded failed\n", []FailedUnit{}},
		{"four fields", "a.service loaded failed failed\n", []FailedUnit{unit("a.service", "")}},
		{"five fields", row, []FailedUnit{unit("a.service", "Thing")}},

		// maxsplit=4 means the description is whatever is left, spacing included.
		{"description with spaces", "a.service loaded failed failed A long thing\n", []FailedUnit{
			unit("a.service", "A long thing"),
		}},
		{"description keeps inner runs", "a.service loaded failed failed A   long    thing\n", []FailedUnit{
			unit("a.service", "A   long    thing"),
		}},
		{"description keeps inner tab", "a.service loaded failed failed A\tthing\n", []FailedUnit{
			unit("a.service", "A\tthing"),
		}},
		{"tab separated row", "a.service\tloaded\tfailed\tfailed\tThing\n", []FailedUnit{
			unit("a.service", "Thing"),
		}},
		// Non-breaking space is whitespace to Python's split, so it separates
		// columns here too.
		{
			"non-breaking space separated",
			strings.ReplaceAll("a.service loaded failed failed Thing\n", " ", nbsp),
			[]FailedUnit{unit("a.service", "Thing")},
		},

		// rstrip runs before the split, so trailing whitespace never reaches the
		// description.
		{"trailing spaces", "a.service loaded failed failed Thing   \n", []FailedUnit{
			unit("a.service", "Thing"),
		}},
		{"trailing tab", "a.service loaded failed failed Thing\t\n", []FailedUnit{
			unit("a.service", "Thing"),
		}},

		{"blank lines", "\n\n" + row + "\n", []FailedUnit{unit("a.service", "Thing")}},
		{"whitespace only line", "   \n", []FailedUnit{}},
		{"no trailing newline", strings.TrimSuffix(row, "\n"), []FailedUnit{unit("a.service", "Thing")}},
		{"crlf", "a.service loaded failed failed Thing\r\n", []FailedUnit{unit("a.service", "Thing")}},

		// splitlines breaks on a vertical tab, so the description is cut there and
		// the remainder is a one-field line that is dropped.
		{"vertical tab", "a.service loaded failed failed Th\ving\n", []FailedUnit{unit("a.service", "Th")}},

		{"unicode", "aé.service loaded failed failed Thïng\n", []FailedUnit{
			unit("aé.service", "Thïng"),
		}},
		{"lowercase unit column", "unit loaded failed failed Thing\n", []FailedUnit{unit("unit", "Thing")}},
		// A unit really named UNIT survives when what follows it is not a space.
		// systemd unit names cannot contain spaces, so the prefix cannot eat a
		// real one.
		{"unit named UNIT", "UNIT\tx\ty\tz\n", []FailedUnit{
			{Name: "UNIT", Load: "x", Active: "y", Sub: "z", Description: ""},
		}},

		{"two rows", "a.service loaded failed failed One\nb.service loaded failed failed Two\n", []FailedUnit{
			unit("a.service", "One"),
			unit("b.service", "Two"),
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseFailedUnits(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseFailedUnits(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

func TestParseShowUserFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    []string
	}{
		{"systemctl/show_user_missing_key.txt", []string{"root", "root", "appuser"}},
		{"systemctl/show_user_three_blocks.txt", []string{"root", "appuser", "root"}},
		{"systemctl/show_user_trailing_blank.txt", []string{"root", "appuser"}},
		{"systemctl/show_user_two_blocks.txt", []string{"root", "appuser"}},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			got := ParseShowUser(fixture.Text(t, tt.fixture))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseShowUser(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

func TestParseShowUser(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", []string{}},
		{"one user", "User=alice\n", []string{"alice"}},
		// An empty or whitespace-only value means the unit runs as root, which is
		// also what systemd means by printing nothing after the =.
		{"empty value", "User=\n", []string{"root"}},
		{"whitespace value", "User=   \n", []string{"root"}},
		{"value is stripped", "User=   alice   \n", []string{"alice"}},
		// A value with a space inside survives, because only the ends are
		// stripped. No such user exists, but the parser does not know that.
		{"value with an inner space", "User=al ice\n", []string{"al ice"}},
		{"unicode value", "User=alïce\n", []string{"alïce"}},
		{"prefix with no value at all", "User=", []string{"root"}},

		{"key absent", "Id=a.service\n", []string{"root"}},
		{"key not first", "Id=a.service\nUser=alice\n", []string{"alice"}},
		{"first key wins", "User=alice\nUser=bob\n", []string{"alice"}},
		// The prefix match is exact, so anything that is not "User=" at column
		// zero reads as absent, i.e. as root.
		{"lowercase key", "user=alice\n", []string{"root"}},
		{"indented key", " User=alice\n", []string{"root"}},
		{"key without the equals", "Users alice\n", []string{"root"}},

		{"two blocks", "User=alice\n\nUser=bob\n", []string{"alice", "bob"}},
		{"three blocks with the middle missing", "User=alice\n\nId=b\n\nUser=carol\n", []string{"alice", "root", "carol"}},
		{"trailing blank", "User=alice\n\n", []string{"alice"}},
		// A blank line is a block separator wherever it falls, so the key after
		// one belongs to a second unit rather than to the first.
		{"blank line inside a block", "Id=a\n\nUser=alice\n", []string{"root", "alice"}},

		{"three newlines", "\n\n\n", []string{}},
		{"two newlines", "\n\n", []string{}},
		{"whitespace only", "   \n", []string{}},
		// strip() uses Python's whitespace set, which includes the non-breaking
		// space, so a block holding only one is empty.
		{"non-breaking space only", nbsp + "\n", []string{}},

		// The block split is on the literal "\n\n" v1 split on, and CRLF output
		// contains none, so every block collapses into the first and only its
		// User= is reported. This is v1's answer, not systemctl's intent.
		{"crlf collapses the blocks", "User=alice\r\n\r\nUser=bob\r\n", []string{"alice"}},
		{"crlf single block", "User=alice\r\n", []string{"alice"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseShowUser(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseShowUser(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// TestFailedParsersReturnEmptyNotNil pins the wire shape: the differential
// harness compares decoded values, and a nil slice marshals to null where
// Python's empty list marshals to [].
func TestFailedParsersReturnEmptyNotNil(t *testing.T) {
	if got := ParseFailedUnits(""); got == nil {
		t.Error(`ParseFailedUnits("") = nil, want an empty slice`)
	}
	if got := ParseShowUser(""); got == nil {
		t.Error(`ParseShowUser("") = nil, want an empty slice`)
	}
}

func FuzzParseFailedUnits(f *testing.F) {
	for _, seed := range []string{
		"",
		"UNIT LOAD ACTIVE SUB DESCRIPTION\na.service loaded failed failed Thing\n",
		"  1 loaded units listed.\n",
		"a b\tc d e f\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, u := range ParseFailedUnits(in) {
			// Every one of the four fixed columns came from a whitespace split, so
			// none can be empty or hold whitespace. The description is exempt: it
			// is the remainder of the line, spacing included.
			for _, col := range []string{u.Name, u.Load, u.Active, u.Sub} {
				if col == "" {
					t.Fatalf("ParseFailedUnits(%q) produced an empty fixed column in %#v", in, u)
				}
				if strings.IndexFunc(col, pytext.IsSpace) >= 0 {
					t.Fatalf("ParseFailedUnits(%q) produced %q, which holds whitespace", in, col)
				}
			}
		}
	})
}

func FuzzParseShowUser(f *testing.F) {
	for _, seed := range []string{
		"",
		"User=root\n\nUser=appuser\n",
		"Id=a\n\n\n",
		"User= \n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, u := range ParseShowUser(in) {
			// Every answer is either the "root" default or a stripped non-empty
			// value, so an empty string or one with whitespace at either end can
			// never be reported as the user a unit runs as.
			if u == "" {
				t.Fatalf("ParseShowUser(%q) reported an empty user", in)
			}
			if strings.TrimFunc(u, pytext.IsSpace) != u {
				t.Fatalf("ParseShowUser(%q) reported %q, which is not stripped", in, u)
			}
		}
	})
}
