package procs

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Expectations measured by running v1's _parse_stat_comm_and_ticks and
// _parse_statm_rss_pages over the same fixture lines.

// statLine is one measured answer. ok=false is v1's None.
type statLine struct {
	comm  string
	ticks int64
	ok    bool
}

// wantStat is the fixture's answers in file order. One /proc/<pid>/stat is a
// single line, so a line per case is the natural shape here and the fixture reads
// as the behaviour table it is.
var wantStat = []statLine{
	{"cat", 0, true},                      // captured from a real /proc
	{"Web Content", 5197, true},           // a comm with a space in it
	{"weird)name", 7, true},               // the reason the scan starts at the LAST ')'
	{"(odd", 11, true},                    // ... and at the FIRST '('
	{"", 11, true},                        // an empty comm is a comm
	{"neg", -11, true},                    // int() takes a sign, so this does
	{"plus", 11, true},                    // ... including a leading '+'
	{"underscore", 56, true},              // int("5_0") is 50, so 50 + 6
	{"arabic", 11, true},                  // int("٥") is 5, so 5 + 6
	{"thirteen fields exactly", 11, true}, // the boundary: 13 fields is enough
	{"", 0, false},                        // twelve is not
	{"", 0, false},                        // four is not
	{"", 0, false},                        // a utime int() rejects
	{"", 0, false},                        // a stime int() rejects
	{"", 0, false},                        // base-10 int() does not read 0x10
	{"", 0, false},                        // no parentheses at all
	{"", 0, false},                        // a ')' with no '('
	{"", 0, false},                        // a '(' after the ')'
}

func TestParseStatCommAndTicksFixture(t *testing.T) {
	lines := fixtureLines(t, "proc/pid/stat.txt")
	if len(lines) != len(wantStat) {
		t.Fatalf("fixture has %d lines, the table has %d", len(lines), len(wantStat))
	}
	for i, line := range lines {
		comm, ticks, ok := ParseStatCommAndTicks(line)
		got := statLine{comm, ticks, ok}
		if diff := cmp.Diff(wantStat[i], got, cmp.AllowUnexported(statLine{})); diff != "" {
			t.Errorf("line %d (%.40q...):\n%s", i+1, line, diff)
		}
	}
}

// wantStatm is the fixture's answers in file order. ok=false is v1's None.
var wantStatm = []struct {
	pages int64
	ok    bool
}{
	{984, true},  // captured from a real /proc
	{4096, true}, // two fields is the minimum
	{984, true},  // surrounding whitespace, which split() eats
	{-5, true},   // int() takes a sign
	{5, true},    // ... including a leading '+'
	{98, true},   // int("9_8") is 98
	{984, true},  // Arabic-Indic digits are digits to int()
	{0, false},   // one field is not enough
	{0, false},   // a resident int() rejects
	{0, false},   // base-10 int() does not read 0x10
}

func TestParseStatmRSSPagesFixture(t *testing.T) {
	lines := fixtureLines(t, "proc/pid/statm.txt")
	if len(lines) != len(wantStatm) {
		t.Fatalf("fixture has %d lines, the table has %d", len(lines), len(wantStatm))
	}
	for i, line := range lines {
		pages, ok := ParseStatmRSSPages(line)
		if pages != wantStatm[i].pages || ok != wantStatm[i].ok {
			t.Errorf("line %d (%q) = (%d, %t), want (%d, %t)",
				i+1, line, pages, ok, wantStatm[i].pages, wantStatm[i].ok)
		}
	}
}

// TestParseStatCommAndTicksReadsACommWithANewlineInIt covers the one shape the
// line-per-case fixture cannot hold: comm is up to 15 bytes of anything, newline
// included, so a stat file is not always one line. v1 reads the whole file and
// scans it for the last ')', which handles this; a parser that had been written
// against the fixture alone might well split on lines first and lose the process.
//
// Measured against v1 directly, since it cannot go through the corpus.
func TestParseStatCommAndTicksReadsACommWithANewlineInIt(t *testing.T) {
	in := "42 (two\nlines) S 1 1 1 0 -1 0 0 0 0 0 7 8 0 0 20 0 1 0 1 1 1 1 1 1 1 0"
	comm, ticks, ok := ParseStatCommAndTicks(in)
	if !ok || comm != "two\nlines" || ticks != 15 {
		t.Errorf("ParseStatCommAndTicks() = (%q, %d, %t), want (%q, 15, true)",
			comm, ticks, ok, "two\nlines")
	}
}

// TestParsersReadNothingFromNothing pins the empty file, which is a real shape for
// a process that exits between the readdir and the read: v1's read_text returns ""
// and both parsers answer None.
func TestParsersReadNothingFromNothing(t *testing.T) {
	if _, _, ok := ParseStatCommAndTicks(""); ok {
		t.Error("ParseStatCommAndTicks(\"\") reported an answer")
	}
	if _, ok := ParseStatmRSSPages(""); ok {
		t.Error("ParseStatmRSSPages(\"\") reported an answer")
	}
}

// TestParsersDepartOnTicksNoInt64Holds covers the magnitude departure both
// functions document. v1 reports an arbitrary-precision integer; there is no
// int64 to put it in, so this reports no answer instead of a wrapped one.
//
// Neither input is reachable from a real /proc -- 10^30 ticks is longer than the
// universe has existed, and 10^30 pages is more memory than exists -- which is why
// they are here rather than in the differential corpus.
func TestParsersDepartOnTicksNoInt64Holds(t *testing.T) {
	huge := "1" + strings.Repeat("0", 30)
	stat := "1 (huge) S 1 1 1 0 -1 0 0 0 0 0 " + huge + " 6 0 0 20 0 1 0 1 1 1 1 1 1 1 0"
	if _, _, ok := ParseStatCommAndTicks(stat); ok {
		t.Error("ParseStatCommAndTicks() reported an answer for a tick count no int64 holds")
	}
	if _, ok := ParseStatmRSSPages("100 " + huge + " 0"); ok {
		t.Error("ParseStatmRSSPages() reported an answer for a page count no int64 holds")
	}
}

// TestDepartureInputsAreReallyTheMagnitudePath guards the case above against
// passing for the wrong reason: the number must be well-formed to int(), so a typo
// that made it a syntax error would leave the test green while testing nothing.
func TestDepartureInputsAreReallyTheMagnitudePath(t *testing.T) {
	huge := "1" + strings.Repeat("0", 30)
	if _, err := pytext.Int(huge); !errors.Is(err, pytext.ErrRange) {
		t.Errorf("pytext.Int(%.8q...) returned %v, want ErrRange, so the guard is untested", huge, err)
	}
}

// TestProcInfoWireShape pins the JSON keys against v1's dataclass field names, so
// a rename here shows up as a test failure rather than as a consumer's missing key.
func TestProcInfoWireShape(t *testing.T) {
	b, err := json.Marshal(ProcInfo{PID: 7, Comm: "sshd", RSSBytes: 4096, CPUPct: 1.5, Cmdline: "sshd -D"})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"pid":7,"comm":"sshd","user":null,"rss_bytes":4096,"cpu_pct":1.5,"cmdline":"sshd -D"}`
	if string(b) != want {
		t.Errorf("ProcInfo marshalled as\n\t%s\nwant\n\t%s", b, want)
	}
}

// fixtureLines splits a behaviour-table fixture the way the oracle adapter does:
// Python's splitlines, empty lines skipped.
func fixtureLines(tb testing.TB, rel string) []string {
	tb.Helper()
	out := []string{}
	for _, line := range pytext.SplitLines(fixture.Text(tb, rel)) {
		if line == "" {
			continue
		}
		out = append(out, line)
	}
	return out
}

func FuzzParseStatCommAndTicks(f *testing.F) {
	for _, line := range fixtureLines(f, "proc/pid/stat.txt") {
		f.Add(line)
	}

	f.Fuzz(func(t *testing.T, text string) {
		comm, ticks, ok := ParseStatCommAndTicks(text)
		if !ok && (comm != "" || ticks != 0) {
			t.Errorf("ParseStatCommAndTicks() returned (%q, %d) alongside ok=false", comm, ticks)
		}
		// The comm is a slice of the input between the first '(' and the last ')',
		// so it cannot contain either bracket at its own boundary positions and it
		// must be a substring of what was read.
		if ok && !strings.Contains(text, comm) {
			t.Errorf("comm %q is not a substring of the input", comm)
		}
	})
}

func FuzzParseStatmRSSPages(f *testing.F) {
	for _, line := range fixtureLines(f, "proc/pid/statm.txt") {
		f.Add(line)
	}

	f.Fuzz(func(t *testing.T, text string) {
		if pages, ok := ParseStatmRSSPages(text); !ok && pages != 0 {
			t.Errorf("ParseStatmRSSPages() returned %d alongside ok=false", pages)
		}
	})
}
