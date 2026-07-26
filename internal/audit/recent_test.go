package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// marshal renders the records the way the differential compares them, which is also
// the only way to assert that a number kept its spelling: json.Number and float64
// are indistinguishable at the reflect level until one of them prints.
func marshal(t *testing.T, v []any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling records: %v", err)
	}
	return string(b)
}

func TestReadRecentSkipsMalformedLines(t *testing.T) {
	got := ReadRecent(fixture.Path("audit/malformed_lines.jsonl"), 200)
	if want := `[{"ok":true},{"ok":false}]`; marshal(t, got) != want {
		t.Errorf("got %s, want %s", marshal(t, got), want)
	}
}

func TestReadRecentMissingFileIsEmpty(t *testing.T) {
	got := ReadRecent(filepath.Join(t.TempDir(), "nope.jsonl"), 200)
	if len(got) != 0 {
		t.Errorf("got %#v, want no records", got)
	}
	// Not nil: v1 returns [] and the caller ranges over it, and a nil slice would
	// marshal as null against Python's [].
	if got == nil {
		t.Error("returned a nil slice; it must marshal as []")
	}
}

// An unreadable file is an empty trail rather than a crash -- v1 tested exists() and
// then raised PermissionError out of a 2-second refresh timer.
func TestReadRecentUnreadableFileIsEmpty(t *testing.T) {
	if got := ReadRecent(t.TempDir(), 200); len(got) != 0 { // a directory: EISDIR
		t.Errorf("got %#v, want no records", got)
	}
}

func TestParseRecords(t *testing.T) {
	tests := []struct {
		name  string
		data  string
		limit int
		want  string
	}{
		{"empty file", "", 200, `[]`},
		{"no trailing newline", `{"a":1}`, 200, `[{"a":1}]`},
		{"blank lines skipped", "{\"a\":1}\n\n   \n{\"a\":2}\n", 200, `[{"a":1},{"a":2}]`},
		{"oldest first", "{\"seq\":1}\n{\"seq\":2}\n{\"seq\":3}\n", 200, `[{"seq":1},{"seq":2},{"seq":3}]`},
		// The tail, so a long trail shows the newest records.
		{"limit takes the tail", "{\"seq\":1}\n{\"seq\":2}\n{\"seq\":3}\n", 2, `[{"seq":2},{"seq":3}]`},
		{"limit of one", "{\"seq\":1}\n{\"seq\":2}\n", 1, `[{"seq":2}]`},
		{"limit above the line count", "{\"seq\":1}\n", 200, `[{"seq":1}]`},
		// lines[-0:] is lines[0:]. A caller passing limit=0 expecting nothing gets
		// everything, and that is v1's behaviour, not a decision made here.
		{"zero limit is every line", "{\"seq\":1}\n{\"seq\":2}\n", 0, `[{"seq":1},{"seq":2}]`},
		// lines[--n:] counts from the front.
		{"negative limit counts from the front", "{\"seq\":1}\n{\"seq\":2}\n{\"seq\":3}\n", -1, `[{"seq":2},{"seq":3}]`},
		{"negative limit past the end", "{\"seq\":1}\n", -5, `[{"seq":1}]`},

		{"not json", "nope\n", 200, `[]`},
		{"truncated json", "{\"a\":1\n", 200, `[]`},
		// A torn final line, which is the case this tolerance exists for.
		{"torn last line", "{\"a\":1}\n{\"a\":2", 200, `[{"a":1}]`},
		// json.loads rejects two values on one line; a Decoder alone would return the
		// first and drop the second silently.
		{"two values on one line", "{\"a\":1} {\"a\":2}\n", 200, `[]`},
		{"trailing garbage", "{\"a\":1} nope\n", 200, `[]`},

		// json.loads is applied to each line and the result appended whatever it is,
		// so a non-object line is a record. The annotation says list[dict]; the code
		// does not.
		{"bare scalar is a record", "5\n", 200, `[5]`},
		{"bare null is a record", "null\n", 200, `[null]`},
		{"bare list is a record", "[1,2]\n", 200, `[[1,2]]`},
		{"bare string is a record", `"hi"`, 200, `["hi"]`},

		// UseNumber earning its place: both of these re-marshal exactly as written,
		// where a float64 would print 1 and 1.2345678901234567e+19.
		{"float keeps its spelling", `{"a":1.0}`, 200, `[{"a":1.0}]`},
		{"integer past 2^53 stays exact", `{"seq":12345678901234567890}`, 200, `[{"seq":12345678901234567890}]`},

		// str.splitlines splits on more than \n, and v1 used it.
		{"crlf", "{\"a\":1}\r\n{\"a\":2}\r\n", 200, `[{"a":1},{"a":2}]`},
		{"form feed", "{\"a\":1}\x0c{\"a\":2}\n", 200, `[{"a":1},{"a":2}]`},
		{"line separator", "{\"a\":1}\u2028{\"a\":2}\n", 200, `[{"a":1},{"a":2}]`},

		// Last-wins, which is json.loads' behaviour too, so a record written twice
		// over does not become an error.
		{"duplicate keys", `{"a":1,"a":2}`, 200, `[{"a":2}]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := marshal(t, ParseRecords([]byte(tc.data), tc.limit)); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// Invalid UTF-8 took v1 down with UnicodeDecodeError from read_text, before json ever
// saw the file. Go's decoder substitutes U+FFFD per bad byte instead, so the record
// survives with the corruption visible in it -- which is what an operator staring at
// a mangled trail needs, and is strictly more than the nothing v1 gave them.
func TestParseRecordsKeepsLinesWithInvalidUTF8(t *testing.T) {
	data := []byte("{\"a\":1}\n{\"b\":\"\xff\xfe\"}\n{\"c\":3}\n")
	want := "[{\"a\":1},{\"b\":\"\ufffd\ufffd\"},{\"c\":3}]"
	if got := marshal(t, ParseRecords(data, 200)); got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestReadRecentReadsFromDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, []byte("{\"seq\":1}\n{\"seq\":2}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := marshal(t, ReadRecent(path, 1)); got != `[{"seq":2}]` {
		t.Errorf("got %s, want the last record", got)
	}
}
