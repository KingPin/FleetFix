package audit

import (
	"bytes"
	"io"
	"os"
)

// Tailer reads a trail forward from where it last stopped.
//
// It replaces ReadRecent for anything that reads the same file repeatedly. v1's
// audit view called read_recent on a 2-second timer, so every tick read the whole
// file, decoded every line, and threw away all but the ones whose seq the view had
// not seen -- work proportional to the trail's total size, forever, on a host whose
// trail only grows. On a fleet machine with a multi-megabyte trail that is several
// megabytes of allocation every two seconds to render at most a handful of new rows.
//
// Here a tick costs the bytes appended since the last one, which on an idle host is
// zero. The seq bookkeeping goes away with it: Next returns the new records, so a
// caller no longer filters by a remembered high-water mark.
//
// Not safe for concurrent use. One Tailer belongs to one reader; the writer it
// follows may be any number of other processes.
type Tailer struct {
	path  string
	limit int

	// off is the byte offset the next read starts at, and info identifies the file
	// that offset belongs to. They only mean anything together: an offset carried
	// onto a different file is a promise about bytes nobody wrote.
	off  int64
	info os.FileInfo
}

// Follow starts a tailer at the end of what it can already see: the first Next
// returns the last limit records, and every Next after that returns what has been
// appended since.
//
// limit bounds each window, not just the first, and follows ReadRecent's Python
// semantics exactly (a limit of zero means every line, a negative one counts from
// the front). Bounding every window matters on a rotation, where the "window" is a
// whole new file. A burst larger than limit within one window loses its oldest
// records -- this is a viewer's tail, deliberately, and a log shipper that must not
// lose a record should be reading the file itself.
func Follow(path string, limit int) *Tailer {
	return &Tailer{path: path, limit: limit}
}

// Next returns the records appended since the previous call, oldest first.
//
// The error is the open or read failure, unlike ReadRecent, which swallows every one
// of them because v1 did. A reader on a timer needs to be able to tell "the trail is
// quiet" from "the trail stopped being readable two hours ago", and those are the
// same empty list. Use errors.Is(err, fs.ErrNotExist) for the ordinary case of a
// trail no writer has opened yet.
//
// A short window is never an error. The records come back through ParseRecords, so a
// torn line is skipped rather than fatal for the same reason it is there.
func (t *Tailer) Next() ([]any, error) {
	f, err := os.Open(t.path) //nolint:gosec // the path is the argument; naming a file is the whole call
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only; a close error has nothing to report

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}

	// Two ways a trail restarts under a reader, and logrotate does both depending on
	// how it is configured. `create` moves the file aside and the writer's next open
	// makes a new one: same path, different inode, and an offset from the old file
	// would start the read somewhere arbitrary in the new one. `copytruncate` keeps
	// the inode and resets the length, so the only evidence is that the file is now
	// shorter than where we stopped. Both restart at zero, which re-shows the last
	// limit records of the new file -- the alternative is a viewer that goes blank
	// after a rotation and stays blank until the next write.
	switch {
	case t.info == nil: // first call
	case !os.SameFile(t.info, info):
		t.off = 0
	case info.Size() < t.off:
		t.off = 0
	}
	t.info = info

	if _, err := f.Seek(t.off, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}

	// Stop at the last newline and leave the remainder for next time. The writer
	// appends a whole line per write, but a reader can still arrive between a crashed
	// process's partial line and the newline that never came -- and consuming that
	// fragment now would parse it as malformed, skip it, and advance past a record
	// that was about to be complete. Held back, it is simply read again with the rest
	// of its line attached.
	//
	// '\n' only, where ParseRecords splits on Python's whole universal-newline set.
	// The difference is not a divergence: the same bytes reach ParseRecords either
	// way, so a record carrying a U+2028 is mangled here exactly as v1 mangled it.
	end := bytes.LastIndexByte(data, '\n')
	if end < 0 {
		// Nothing complete yet. The offset does not move, so nothing is lost.
		return []any{}, nil
	}
	data = data[:end+1]
	t.off += int64(len(data))

	return ParseRecords(data, t.limit), nil
}
