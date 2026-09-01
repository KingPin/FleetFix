// Package logsqueeze registers the log-reclaim domain's check.
//
// One question, and it is the one an operator asks after disk.usage goes red on
// a host whose disk is full of logs: how much of /var/log is sitting there
// uncompressed, and which file is the worst of it. v1 asked it in
// modules/log_squeeze/gzip_inplace.py as the first half of a workflow whose
// second half gzips the answer.
//
// # Why this grades nothing
//
// internal/threshold has no rule for it, and inventing one here would be picking
// a number in the wrong place -- the bounds live there so an operator can
// override them per host, and a bound that exists only in this file cannot be
// overridden at all. It would also be the wrong question twice over: a gigabyte
// of uncompressed logs is unremarkable on a host with a terabyte free and
// urgent on one with two gigabytes, and disk.usage already grades that. So this
// check reports and narrates, and its status is ok whenever the walk completed.
//
// # What is deliberately not here
//
// The writer check. v1 runs `lsof -Fan -- <path>` before squeezing a file,
// because gzipping a log a process still holds open for write truncates a
// journal mid-flush -- and internal/core/logsqueeze.LsofHasWriter is that
// parser, ported. It is not called from here: one subprocess per candidate is a
// cost this check has no use for, and the answer is stale the moment it is
// printed. It belongs to the squeeze action, which asks it about the one file it
// is about to touch, immediately before touching it.
//
// The squeeze itself is likewise absent. This domain is read-only; writing is a
// Tier 2 action behind the confirm gate.
package logsqueeze

import (
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
)

// CandidatesID is the domain's check id. Public API: it appears in checks[], in
// --check selectors, and in whatever Ansible an operator writes against it.
const CandidatesID check.ID = "logsqueeze.candidates"

// Metric names, likewise public.
//
// Named for their subject rather than the domain, which is the convention the
// system domain set with cpu.* and mem.*: what these count is logs, and
// "logsqueeze" is the name of the workflow that acts on them.
//
// Not "reclaimable". gzip on a text log usually returns most of it and
// occasionally does not, and this check compresses nothing, so a number under
// that name would be a guess wearing a measurement's clothes. These are the
// bytes that are currently uncompressed; how many come back is what the squeeze
// reports, after it has done the work.
const (
	BytesMetric   = "logs.uncompressed_bytes"
	FilesMetric   = "logs.uncompressed_files"
	LargestMetric = "logs.largest_bytes"
)

// DefaultRoot is v1's DEFAULT_ROOTS, which holds this one directory
// (modules/log_squeeze/gzip_inplace.py:29).
//
// One root rather than v1's tuple. Nothing in v1 ever passed a second, the TUI
// had no control for it, and a list that is always one element long is a
// parameter pretending to be a feature. A caller wanting another directory sets
// Root; a caller wanting two runs the check twice.
const DefaultRoot = "/var/log"

// DefaultMinBytes is v1's DEFAULT_MIN_BYTES: ten mebibytes
// (modules/log_squeeze/gzip_inplace.py:30). Below it, squeezing a file is not
// worth the operator's attention or the write.
const DefaultMinBytes int64 = 10 * 1024 * 1024

// budget caps the walk. v1 put no timeout on it at all, which is fine for a
// button an operator pressed and wrong for a check a cron job runs: /var/log on
// a host with a runaway logger holds a lot of directory entries, and a check
// that never returns holds the whole report open.
const budget = 20 * time.Second

// stepCap bounds how many candidates get a line of their own.
//
// The remainder is stated rather than dropped quietly, and data[] carries every
// candidate regardless. Ten is what the services domain settled on for the same
// question.
const stepCap = 10

// Source is where the logs are read from.
//
// Root is a path string and FS is the reader, rather than one or the other,
// because both answers are needed and neither derives from the other: fs.FS
// paths are relative by contract, so the walk cannot name the files it found
// without the root, and a test that stages a tree has no directory on disk to
// point a root at.
type Source struct {
	// Root is the directory walked, and the prefix every reported path carries.
	// Empty means DefaultRoot.
	Root string

	// FS reads the tree. Nil means the live filesystem rooted at Root.
	FS fs.FS

	// MinBytes is the size a file must reach to be worth reporting. Zero means
	// DefaultMinBytes; a negative value reports every file the name test matched,
	// which is what a caller asking for all of them would mean by it.
	MinBytes int64
}

// New returns a Source reading the live /var/log at v1's threshold.
func New() Source {
	return Source{Root: DefaultRoot, MinBytes: DefaultMinBytes}
}

// root is the directory to walk and to name files under.
func (s Source) root() string {
	if s.Root == "" {
		return DefaultRoot
	}
	return s.Root
}

// fsys is the reader, defaulted to the live filesystem under root.
func (s Source) fsys() fs.FS {
	if s.FS != nil {
		return s.FS
	}
	return os.DirFS(s.root())
}

// minBytes is the size floor, with zero meaning v1's default.
//
// Zero cannot mean "every file" because zero is what a Source built by a struct
// literal has, and a caller who forgot the field should get the shipped
// behaviour rather than a walk that reports every empty rotated log on the host.
// A caller who really wants all of them says so with -1.
func (s Source) minBytes() int64 {
	if s.MinBytes == 0 {
		return DefaultMinBytes
	}
	return s.MinBytes
}

// Checks returns the domain's checks, reading through the given source.
//
// A constructor rather than registration by init(), for the reason
// builtin.Checks gives: what a build runs must not depend on what the linker
// kept.
func Checks(src Source) []check.Check {
	return []check.Check{candidates{src: src}}
}

func gauge(name string, value float64, unit, help string) check.Metric {
	return check.Metric{
		Name:   name,
		Value:  value,
		Unit:   unit,
		Labels: map[string]string{},
		Kind:   check.Gauge,
		Help:   help,
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// humanBytes renders a byte count for a summary line. Binary steps, one decimal
// past the first unit -- the same rendering the docker and system domains use,
// so two lines of one report do not disagree about what a gigabyte is.
func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	size := float64(n)
	i := 0
	for ; size >= 1024 && i < len(units)-1; i++ {
		size /= 1024
	}
	if i == 0 {
		return fmt.Sprintf("%d B", int64(size))
	}
	return fmt.Sprintf("%.1f %s", size, units[i])
}
