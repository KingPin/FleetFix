// Package logsqueeze holds the reclaim-log-space logic: deciding whether a log
// file is safe to compress in place, and parsing what the tools that answer
// that question print.
package logsqueeze

import (
	"strings"
	"unicode/utf8"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// LsofHasWriter reports whether any process holds the file open for writing,
// given the output of `lsof -F a <path>`.
//
// This is the gate in front of compressing a log in place. A file being read is
// fine -- the reader keeps its own offset into the old inode -- but a writer
// appending to a file that has just been replaced by its .gz writes into a
// deleted inode, and those bytes are lost with no error anywhere.
//
// The a field is lsof's access mode: r, w, or u for read-write. Anything
// containing a w or a u is a writer. The test is deliberately as loose as v1's,
// which is to say it looks for those letters anywhere in the value rather than
// matching the mode exactly, so an unexpected mode string still errs towards
// refusing to compress. Case matters: an uppercase W is not a writer to either
// implementation.
//
// Only the first rune of a line is the tag, so a lone "w" line is not a writer
// -- its tag is w and there is no value at all.
func LsofHasWriter(output string) bool {
	for _, line := range pytext.SplitLines(output) {
		if line == "" {
			continue
		}
		tag, size := utf8.DecodeRuneInString(line)
		if tag == 'a' && strings.ContainsAny(line[size:], "wu") {
			return true
		}
	}
	return false
}
