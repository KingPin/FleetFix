// Package procs ranks the processes on this host by memory or CPU, read straight
// out of /proc rather than out of `ps`.
//
// Not shelling out is deliberate on v1's part and worth keeping: one open() per
// process gives a consistent snapshot, and it works on a host whose PATH has been
// cut down to nothing, which is exactly the kind of host someone is logged into
// when they reach for this.
//
// What lives here at M2 is the two parsers and the result struct. The walk that
// feeds them is I/O and lands with the collectors.
package procs

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// ProcInfo is one process as the ranker reports it.
//
// User is a pointer because a uid with no passwd entry is an ordinary answer on a
// container host -- the process is real and its owner has no name -- and reporting
// the absence beats inventing "root" or printing the number twice.
type ProcInfo struct {
	PID      int64   `json:"pid"`
	Comm     string  `json:"comm"`
	User     *string `json:"user"`
	RSSBytes int64   `json:"rss_bytes"`
	CPUPct   float64 `json:"cpu_pct"`
	Cmdline  string  `json:"cmdline"`
}

// ParseStatCommAndTicks pulls the process name and its total CPU ticks out of one
// /proc/<pid>/stat. ok is false for a line this cannot read, which the caller
// treats as "this process is not rankable" rather than as an error.
//
// The format is space-separated with one exception that makes naive splitting
// wrong: field 2 is the comm, parenthesised, and the kernel puts up to 15 bytes of
// whatever the process called itself in there -- spaces and parentheses included.
// So the split starts after the *last* ')', which is why a process named
// "weird)name" parses and a process named "(odd" keeps its leading paren.
//
// Ticks, not seconds: utime + stime are in clock ticks, and turning them into a
// percentage needs two samples and the interval between them, which is the
// caller's job. Summing them here is v1's choice and keeps the pair that has to
// be read from the same line being read once.
//
// Negative values are returned as they are read. No kernel writes one, but int()
// accepts a leading '-' and so does this, and a parser that silently clamped would
// be inventing a number rather than reporting the file.
//
// One departure, kept out of the corpus because no /proc produces it: a tick count
// above 2^63-1 is unreadable here and an arbitrary-precision integer to v1, so
// this reports no answer where v1 reports a huge one. At 100 ticks a second that
// is about three billion years of CPU time.
func ParseStatCommAndTicks(text string) (comm string, ticks int64, ok bool) {
	end := strings.LastIndex(text, ")")
	if end < 0 {
		return "", 0, false
	}
	start := strings.Index(text, "(")
	if start < 0 || start > end {
		return "", 0, false
	}
	comm = text[start+1 : end]

	// rest[0] is field 3, the state, so utime (field 14) is rest[11] and stime
	// (field 15) is rest[12].
	rest := pytext.Fields(text[end+1:])
	if len(rest) < 13 {
		return "", 0, false
	}
	utime, err := pytext.Int(rest[11])
	if err != nil {
		return "", 0, false
	}
	stime, err := pytext.Int(rest[12])
	if err != nil {
		return "", 0, false
	}
	return comm, utime + stime, true
}

// ParseStatmRSSPages pulls the resident set size, in pages, out of one
// /proc/<pid>/statm. ok is false for a line this cannot read.
//
// The fields are: size resident shared text lib data dt. Resident is the second,
// and it is pages rather than bytes -- multiplying by the page size is the
// caller's job, because the page size is a property of the host and not of the
// file.
//
// The same magnitude departure as ParseStatCommAndTicks applies, and is just as
// unreachable: a resident set above 2^63-1 pages is more memory than exists.
func ParseStatmRSSPages(text string) (pages int64, ok bool) {
	parts := pytext.Fields(text)
	if len(parts) < 2 {
		return 0, false
	}
	n, err := pytext.Int(parts[1])
	if err != nil {
		return 0, false
	}
	return n, true
}
