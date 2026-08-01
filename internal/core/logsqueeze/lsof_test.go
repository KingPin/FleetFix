package logsqueeze

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// Expectations come from src/fleetfix/modules/log_squeeze/gzip_inplace.py's
// _lsof_has_writer under CPython 3.14.6, fed the same literals.

func TestLsofHasWriterFixtures(t *testing.T) {
	tests := []struct {
		fixture string
		want    bool
	}{
		{"lsof/open_read_only.txt", false},
		{"lsof/open_rw_mode.txt", true},
		{"lsof/open_writer.txt", true},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			if got := LsofHasWriter(fixture.Text(t, tt.fixture)); got != tt.want {
				t.Errorf("LsofHasWriter(%s) = %v, want %v", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestLsofHasWriter(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		// -F a is asked for, but the other fields come back too and none of them
		// is the access mode.
		{"no access lines", "p100\nf3\nn/var/log/x.log\n", false},
		{"read only", "ar\n", false},
		{"write", "aw\n", true},
		{"update", "au\n", true},
		{"read write", "arw\n", true},
		{"access empty", "a\n", false},
		// The test is a substring search, not an exact mode match, so a mode
		// neither implementation anticipates still refuses to compress. That is
		// the safe direction: the cost of a false writer is a log left
		// uncompressed, the cost of a missed one is written bytes lost into a
		// deleted inode.
		{"w elsewhere in the value", "axxwxx\n", true},
		{"u elsewhere in the value", "axxuxx\n", true},
		// lsof has no uppercase access mode, and the comparison is exact, so this
		// reads as not-a-writer in both implementations.
		{"uppercase w is not a writer", "aW\n", false},
		// Only the a field is consulted; a w under any other tag is a value, not
		// a mode.
		{"another tag holding a w", "fw\n", false},
		{"second line is the writer", "ar\naw\n", true},
		{"blank lines skipped", "\n\naw\n", true},
		{"no trailing newline", "aw", true},
		{"carriage return endings", "ar\r\naw\r\n", true},
		// splitlines splits on \f as well, so what looks like one field is two.
		{"form feed boundary", "ar\x0caw\n", true},
		// The tag is the first rune and the value is everything after it, so a
		// line of just "w" has tag w and no value at all.
		{"line is just w", "w\n", false},
		// U+0430 CYRILLIC SMALL LETTER A. It looks like the tag and is not it;
		// decoding a byte instead of a rune would leave a half-rune tag that is
		// not 'a' either, so this passes for the wrong reason unless the whole
		// rune is consumed -- which is what makes the value "w" rather than the
		// second byte of the а.
		{"unicode a lookalike", "аw\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := LsofHasWriter(tt.in); got != tt.want {
				t.Errorf("LsofHasWriter(%q) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

// A writer needs both an a field and one of the two letters. Necessary rather
// than sufficient, but a parser that answered true without them would be
// declaring every log unsafe to compress, which silently turns the whole
// feature off.
func FuzzLsofHasWriter(f *testing.F) {
	for _, name := range []string{"open_read_only", "open_rw_mode", "open_writer"} {
		f.Add(fixture.Text(f, "lsof/"+name+".txt"))
	}
	f.Fuzz(func(t *testing.T, in string) {
		if !LsofHasWriter(in) {
			return
		}
		if !strings.Contains(in, "a") {
			t.Errorf("writer reported for input with no a field: %q", in)
		}
		if !strings.ContainsAny(in, "wu") {
			t.Errorf("writer reported for input with no w or u: %q", in)
		}
	})
}
