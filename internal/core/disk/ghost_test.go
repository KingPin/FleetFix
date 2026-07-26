package disk

import (
	"errors"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
	"github.com/google/go-cmp/cmp"
)

// Every expectation below was produced by feeding the same literal to
// src/fleetfix/modules/disk/ghost.py under CPython 3.14.6 and printing the
// dataclasses, not by reading the Go and writing down what it does. Change one
// by re-running Python.

// lsofGroup is the process header the single-file cases share: pid 100, command
// syslogd, user root.
const lsofGroup = "p100\ncsyslogd\nuroot\n"

// lsofGhost is a ghost file under lsofGroup.
func lsofGhost(fd string, size int64, path string) GhostFile {
	return GhostFile{PID: 100, Command: "syslogd", User: "root", FD: fd, SizeBytes: size, Path: path}
}

func TestParseLsofFieldOutputFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []GhostFile
	}{
		{
			// Two processes, and in the first one a live file before the deleted
			// one -- so this also pins that f ends a file without ending the group.
			name:    "ghost files",
			fixture: "lsof/ghost_files.txt",
			want: []GhostFile{
				{PID: 812, Command: "sshd", User: "root", FD: "4", SizeBytes: 12345678, Path: "/var/log/journal/abc/system.journal (deleted)"},
				{PID: 1234, Command: "postgres", User: "postgres", FD: "9", SizeBytes: 99999, Path: "/var/lib/postgresql/14/main/pg_log.deleted"},
			},
		},
		{
			// s is "oops". The file is still a ghost -- the link count is what
			// decides that -- it just has no size to report.
			name:    "garbage size",
			fixture: "lsof/ghost_garbage_size.txt",
			want:    []GhostFile{{PID: 1, Command: "bad", User: "0", FD: "0", SizeBytes: 0, Path: "/tmp/x"}},
		},
		{
			name:    "held with nonzero links",
			fixture: "lsof/ghost_held_nonzero_links.txt",
			want:    []GhostFile{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseLsofFieldOutput(fixture.Text(t, tt.fixture))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseLsofFieldOutput(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

func TestParseLsofFieldOutput(t *testing.T) {
	none := []GhostFile{}
	tests := []struct {
		name string
		in   string
		want []GhostFile
	}{
		{"empty", "", none},
		{"blank lines only", "\n\n", none},
		// An empty line is skipped rather than treated as a field with an empty
		// tag, so it does not end the file being accumulated.
		{"blank line inside a group", lsofGroup + "f3\ns1\n\nL0\nn/a\n", []GhostFile{lsofGhost("3", 1, "/a")}},
		// The blank-line skip only skips *empty* lines, so these become fields
		// under the tags ' ' and '\t'. Neither is L, so the link count defaults
		// to one and nothing is reported -- but the accumulator was non-empty,
		// which is the part worth pinning.
		{"whitespace only", "   \n\t\n", none},
		{"one ghost", lsofGroup + "f3\ns1024\nL0\nn/var/log/old.log\n", []GhostFile{lsofGhost("3", 1024, "/var/log/old.log")}},
		{"link count one is not a ghost", lsofGroup + "f3\ns1024\nL1\nn/var/log/live.log\n", none},
		{"link count missing defaults to one", lsofGroup + "f3\ns1024\nn/var/log/live.log\n", none},
		{"link count not a number defaults to one", lsofGroup + "f3\ns1024\nLbanana\nn/x\n", none},
		// Python's int() strips surrounding whitespace, accepts a sign, leading
		// zeroes, underscore separators and non-ASCII decimal digits. Every one
		// of these is zero and so every one of them is a ghost; pytext.Int is
		// what makes that true in Go.
		{"link count with spaces", lsofGroup + "f3\ns1\nL 0 \nn/x\n", []GhostFile{lsofGhost("3", 1, "/x")}},
		{"link count leading zeroes", lsofGroup + "f3\ns1\nL00\nn/x\n", []GhostFile{lsofGhost("3", 1, "/x")}},
		{"link count signed zero", lsofGroup + "f3\ns1\nL-0\nn/x\n", []GhostFile{lsofGhost("3", 1, "/x")}},
		{"link count plus zero", lsofGroup + "f3\ns1\nL+0\nn/x\n", []GhostFile{lsofGhost("3", 1, "/x")}},
		{"link count underscore", lsofGroup + "f3\ns1\nL0_0\nn/x\n", []GhostFile{lsofGhost("3", 1, "/x")}},
		{"link count unicode zero", lsofGroup + "f3\ns1\nL٠\nn/x\n", []GhostFile{lsofGhost("3", 1, "/x")}},
		// An empty value is a ValueError, not a zero, so it lands on the default
		// of one link and the file is not reported.
		{"link count empty", lsofGroup + "f3\ns1\nL\nn/x\n", none},
		{"size missing", lsofGroup + "f3\nL0\nn/x\n", []GhostFile{lsofGhost("3", 0, "/x")}},
		{"size not a number", lsofGroup + "f3\nsbanana\nL0\nn/x\n", []GhostFile{lsofGhost("3", 0, "/x")}},
		{"size negative", lsofGroup + "f3\ns-5\nL0\nn/x\n", []GhostFile{lsofGhost("3", -5, "/x")}},
		{"size with spaces", lsofGroup + "f3\ns 7 \nL0\nn/x\n", []GhostFile{lsofGhost("3", 7, "/x")}},
		{"size underscore", lsofGroup + "f3\ns1_000\nL0\nn/x\n", []GhostFile{lsofGhost("3", 1000, "/x")}},
		{"size unicode digits", lsofGroup + "f3\ns٤٢\nL0\nn/x\n", []GhostFile{lsofGhost("3", 42, "/x")}},
		// f ends the previous file without ending the process, so both inherit
		// the one command and user.
		{
			"two files in one group",
			lsofGroup + "f3\ns1\nL0\nn/a\nf4\ns2\nL0\nn/b\n",
			[]GhostFile{lsofGhost("3", 1, "/a"), lsofGhost("4", 2, "/b")},
		},
		// p does end the process, and clears c and u with it -- the second group
		// reports empty strings rather than inheriting syslogd/root.
		{
			"second group resets command and user",
			lsofGroup + "f3\ns1\nL0\nn/a\n" + "p200\nf4\ns2\nL0\nn/b\n",
			[]GhostFile{lsofGhost("3", 1, "/a"), {PID: 200, FD: "4", SizeBytes: 2, Path: "/b"}},
		},
		{
			"second group with its own command",
			lsofGroup + "f3\ns1\nL0\nn/a\n" + "p200\nctail\nubob\nf4\ns2\nL0\nn/b\n",
			[]GhostFile{lsofGhost("3", 1, "/a"), {PID: 200, Command: "tail", User: "bob", FD: "4", SizeBytes: 2, Path: "/b"}},
		},
		// An unreadable pid is zero, and the c line after it still applies: the
		// p branch clears command *before* the c line is read, not after.
		{"pid not a number", "pbanana\nctail\nf3\ns1\nL0\nn/a\n", []GhostFile{{Command: "tail", FD: "3", SizeBytes: 1, Path: "/a"}}},
		{"pid negative", "p-1\nf3\ns1\nL0\nn/a\n", []GhostFile{{PID: -1, FD: "3", SizeBytes: 1, Path: "/a"}}},
		{"pid unicode digits", "p٤٢\nf3\ns1\nL0\nn/a\n", []GhostFile{{PID: 42, FD: "3", SizeBytes: 1, Path: "/a"}}},
		// lsof always opens with a p line; a file described before one is
		// reported anyway, owned by pid 0.
		{"fields before any pid", "f3\ns1\nL0\nn/a\n", []GhostFile{{FD: "3", SizeBytes: 1, Path: "/a"}}},
		{"fd only", lsofGroup + "f3\n", none},
		{"fd empty value", lsofGroup + "f\ns1\nL0\nn/a\n", []GhostFile{lsofGhost("", 1, "/a")}},
		{"name empty value", lsofGroup + "f3\ns1\nL0\nn\n", []GhostFile{lsofGhost("3", 1, "")}},
		// An unrecognised tag is stored under itself and never read. It matters
		// only because storing it makes the accumulator non-empty, which is what
		// decides whether a flush does anything at all.
		{"unknown tag is kept but unused", lsofGroup + "f3\nXzzz\ns1\nL0\nn/a\n", []GhostFile{lsofGhost("3", 1, "/a")}},
		{"duplicate tag last wins", lsofGroup + "f3\ns1\ns2\nL0\nn/a\n", []GhostFile{lsofGhost("3", 2, "/a")}},
		// The value is the rest of the line verbatim, so a path may hold spaces
		// and tabs. lsof does not escape them and neither side tries to.
		{"path with spaces", lsofGroup + "f3\ns1\nL0\nn/var/log/my log.log\n", []GhostFile{lsofGhost("3", 1, "/var/log/my log.log")}},
		{"path with a tab", lsofGroup + "f3\ns1\nL0\nn/var/log/my\tlog\n", []GhostFile{lsofGhost("3", 1, "/var/log/my\tlog")}},
		{"no trailing newline", lsofGroup + "f3\ns1\nL0\nn/a", []GhostFile{lsofGhost("3", 1, "/a")}},
		// splitlines treats \r\n as one boundary, so nothing carries a stray \r.
		{"carriage return endings", "p100\r\nf3\r\ns1\r\nL0\r\nn/a\r\n", []GhostFile{{PID: 100, FD: "3", SizeBytes: 1, Path: "/a"}}},
		// It also splits on \f, \v, \x1c-\x1e, U+0085, U+2028 and U+2029, none of
		// which lsof emits and all of which therefore silently start a field.
		{
			"form feed boundary",
			lsofGroup + "f3\ns1\nL0\nn/a\x0cf4\ns2\nL0\nn/b\n",
			[]GhostFile{lsofGhost("3", 1, "/a"), lsofGhost("4", 2, "/b")},
		},
		// The tag is one rune, so the whole é is consumed and "zz" is its value.
		// Reading a byte instead would leave a broken half-rune as the tag.
		{"non ascii tag", lsofGroup + "f3\ns1\nL0\nézz\nn/a\n", []GhostFile{lsofGhost("3", 1, "/a")}},
		// A bare "L" is the tag with an empty value, which resets the link count
		// to its unparseable default of one and un-ghosts a file already
		// described as deleted.
		{"tag is a single char line", lsofGroup + "f3\ns1\nL0\nn/a\nL\n", none},
		{"group with no files", lsofGroup, none},
		// The trailing p flushes the file before it and then describes nothing,
		// so pid 200 never appears.
		{"trailing p with no files", lsofGroup + "f3\ns1\nL0\nn/a\np200\n", []GhostFile{lsofGhost("3", 1, "/a")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseLsofFieldOutput(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseLsofFieldOutput(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// TestParseLsofFieldOutputHugeNumbersFallBack pins the one place this parser
// knowingly answers differently from v1.
//
// Python's ints have no ceiling, so it reports a 30-digit pid and a 30-digit
// size as themselves. int64 cannot, and the choice is between a wrong number
// and no number: both fall back to the value the unparseable case already
// uses, so a size no host can produce reports as no size rather than as a
// truncated one that would be summed into a reclaim estimate. Only lsof output
// that has been tampered with or corrupted can reach this.
func TestParseLsofFieldOutputHugeNumbersFallBack(t *testing.T) {
	huge := strings.Repeat("9", 30)

	got := ParseLsofFieldOutput("p" + huge + "\nf3\ns" + huge + "\nL0\nn/x\n")
	want := []GhostFile{{PID: 0, FD: "3", SizeBytes: 0, Path: "/x"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("huge pid and size mismatch (-want +got):\n%s", diff)
	}

	// The link count needs no departure: out of range is not zero in either
	// language, so both agree the file is not a ghost.
	if got := ParseLsofFieldOutput(lsofGroup + "f3\ns1\nL" + huge + "\nn/x\n"); len(got) != 0 {
		t.Errorf("huge link count reported %d ghosts, want 0", len(got))
	}
}

// The fallbacks above are only equivalent to Python's if a number this parser
// rejects was rejected for being too large rather than for being malformed --
// a malformed one is a ValueError in Python and takes the same branch anyway,
// but a *silently different* parse would not. Pin that pytext.Int agrees with
// int() on the shapes lsof fields actually carry.
func TestLsofNumbersOnlyFailAsOutOfRange(t *testing.T) {
	for _, s := range []string{"0", "1", "-5", " 7 ", "1_000", "00", "-0", "+0", "٠", "٤٢", strings.Repeat("9", 30)} {
		if _, err := pytext.Int(s); err != nil && !errors.Is(err, pytext.ErrRange) {
			t.Errorf("pytext.Int(%q) failed as %v, want success or a range error", s, err)
		}
	}
}

func TestParseLsofFieldOutputReturnsEmptyNotNil(t *testing.T) {
	if got := ParseLsofFieldOutput(""); got == nil {
		t.Error("ParseLsofFieldOutput returned nil; the JSON would be null where Python's is []")
	}
}

func TestTotalBytes(t *testing.T) {
	tests := []struct {
		name  string
		files []GhostFile
		want  int64
	}{
		{"none", []GhostFile{}, 0},
		{"fixture", ParseLsofFieldOutput(fixture.Text(t, "lsof/ghost_files.txt")), 12445677},
		// Sizes are whatever lsof said, including the nonsense ones, and a
		// negative term really does subtract.
		{"negative size", []GhostFile{{SizeBytes: 10}, {SizeBytes: -4}}, 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TotalBytes(tt.files); got != tt.want {
				t.Errorf("TotalBytes(%v) = %d, want %d", tt.files, got, tt.want)
			}
		})
	}
}

// A ghost needs an L line: the default link count is one, so no input without
// one can produce a file. Necessary rather than sufficient, but it is the
// invariant that a flipped default would break, and a flipped default reports
// every open log on the host as reclaimable.
func FuzzParseLsofFieldOutput(f *testing.F) {
	for _, name := range []string{"ghost_files", "ghost_garbage_size", "ghost_held_nonzero_links"} {
		f.Add(fixture.Text(f, "lsof/"+name+".txt"))
	}
	f.Fuzz(func(t *testing.T, in string) {
		files := ParseLsofFieldOutput(in)
		if len(files) > 0 && !strings.Contains(in, "L") {
			t.Errorf("%d ghosts from input with no L field: %q", len(files), in)
		}
		TotalBytes(files)
	})
}
