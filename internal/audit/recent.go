package audit

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// ReadRecent returns the last limit records, oldest first, skipping lines that are
// not JSON.
//
// A malformed line is skipped rather than fatal because the file is append-only from
// possibly-concurrent processes: the last line can be torn mid-write, and a reader
// that gave up on it would show the operator nothing.
//
// The element type is any, not a record struct or a map, because that is what v1
// returns: json.loads is applied to each line and whatever it produces is appended,
// so a line holding a bare `5` or `[1,2]` becomes an entry despite the annotation
// saying list[dict]. A typed accessor belongs above this, over the same values.
//
// This is the straight port, and it is what the differential compares. It re-reads
// the whole file, which at a 2-second refresh over a multi-megabyte trail is the
// allocation problem M4 replaces with an offset-and-inode tailer -- but the
// replacement has to agree with this first.
func ReadRecent(path string, limit int) []any {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the argument; naming a file is the whole call
	if err != nil {
		// v1 tested path.exists() and returned [], so an audit log that existed and
		// could not be read raised PermissionError out of the view's 2-second refresh
		// timer instead. Every read failure is an empty trail here; telling the
		// operator which kind it was is `doctor`'s job, and it stats the path itself.
		//
		// Invalid UTF-8 is not one of these: it took v1 down at read_text, and here it
		// reaches the decoder, which substitutes U+FFFD and keeps the record.
		return []any{}
	}
	return ParseRecords(data, limit)
}

// ParseRecords is ReadRecent's parse half, split out so the M4 tailer can feed it a
// window of bytes instead of a whole file.
func ParseRecords(data []byte, limit int) []any {
	lines := pytext.SplitLines(string(data))
	// lines[-limit:] -- and Python's -0 is 0, so a limit of zero is every line, not
	// none. A negative limit counts from the front instead. Both are warts, both are
	// reachable from v1's keyword argument, so both are reproduced.
	switch {
	case limit > 0 && len(lines) > limit:
		lines = lines[len(lines)-limit:]
	case limit < 0 && -limit < len(lines):
		lines = lines[-limit:]
	}

	out := []any{}
	for _, line := range lines {
		line = strings.TrimFunc(line, pytext.IsSpace)
		if line == "" {
			continue
		}
		if v, ok := decodeJSON(line); ok {
			out = append(out, v)
		}
	}
	return out
}

// decodeJSON is json.loads for one line: the whole line must be one JSON value.
//
// UseNumber, because the numbers are compared against Python's. json.loads keeps
// 1.0 a float and 12345678901234567890 an exact int, and decoding into a float64
// would re-marshal them as 1 and 1.2345678901234567e+19 -- the first a cosmetic
// diff, the second a silently wrong sequence number.
func decodeJSON(line string) (any, bool) {
	dec := json.NewDecoder(strings.NewReader(line))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	// Trailing content is a decode error to json.loads but not to a Decoder, which
	// would otherwise return the first of two values on one line.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return v, true
}
