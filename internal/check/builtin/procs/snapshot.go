package procs

import (
	"cmp"
	"context"
	"io/fs"
	"slices"
	"strconv"
	"strings"
	"time"

	coreprocs "github.com/KingPin/FleetFix/v2/internal/core/procs"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// A snapshot is one pass over /proc, already ranked.
//
// Both orderings and the count come out of the same pass, which is the whole
// argument for this domain having one check: the two lists describe the same
// moment, and an operator reading a pid off both is reading it off one process
// table.
type snapshot struct {
	// ByRSS and ByCPU are the same rows in two orders, not two sets of rows.
	ByRSS []coreprocs.ProcInfo
	ByCPU []coreprocs.ProcInfo

	// Unreadable counts processes that were there and would not be read. A
	// process that exited mid-walk is not one of them; see readStat.
	Unreadable int

	// Elapsed is how long the two samples were actually apart, which is what the
	// percentages were divided by.
	Elapsed time.Duration
}

// take reads /proc twice, a sample interval apart, and ranks what it found.
//
// Two passes because a CPU percentage is a rate, and one reading of a counter is
// not one. v1 does the same, and this keeps its shape: sample every process's
// tick counter, wait, then read everything -- so the second pass is what decides
// which processes exist, and a process that started during the interval is
// reported at zero per cent rather than dropped.
func (s Source) take(ctx context.Context) (snapshot, error) {
	first, err := s.sample(ctx)
	if err != nil {
		return snapshot{}, err
	}

	started := s.now()
	if err := s.wait(ctx); err != nil {
		return snapshot{}, err
	}
	elapsed := s.now().Sub(started)

	rows, unreadable, err := s.read(ctx, first, elapsed)
	if err != nil {
		return snapshot{}, err
	}

	byRSS := slices.Clone(rows)
	slices.SortFunc(byRSS, func(a, b coreprocs.ProcInfo) int {
		if c := cmp.Compare(b.RSSBytes, a.RSSBytes); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})

	byCPU := slices.Clone(rows)
	slices.SortFunc(byCPU, func(a, b coreprocs.ProcInfo) int {
		if c := cmp.Compare(b.CPUPct, a.CPUPct); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})

	return snapshot{ByRSS: byRSS, ByCPU: byCPU, Unreadable: unreadable, Elapsed: elapsed}, nil
}

// wait pauses for the sample interval, or returns early if the run was cancelled.
//
// A plain sleep would ignore the runner's deadline, and the deadline only cuts off
// the result rather than the goroutine -- so a cancelled run would still be paying
// for this one long after its report went out.
func (s Source) wait(ctx context.Context) error {
	t := time.NewTimer(s.sampleInterval())
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// sample reads every process's CPU tick counter and nothing else.
//
// Failures are silent here, deliberately: this pass exists only to have something
// to subtract from, a process missing from it is reported at zero per cent by the
// second pass, and counting the same denial in both passes would make the summary
// state a number nobody could reconcile with the list underneath it.
func (s Source) sample(ctx context.Context) (map[int64]int64, error) {
	pids, err := s.pids()
	if err != nil {
		return nil, err
	}
	out := make(map[int64]int64, len(pids))
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text, err := hostfs.ReadFileLossy(s.fsys(), pidPath(pid, "stat"))
		if err != nil {
			continue
		}
		if _, ticks, ok := coreprocs.ParseStatCommAndTicks(text); ok {
			out[pid] = ticks
		}
	}
	return out, nil
}

// read is the second pass: everything this domain reports, per process.
func (s Source) read(ctx context.Context, prev map[int64]int64, elapsed time.Duration) ([]coreprocs.ProcInfo, int, error) {
	pids, err := s.pids()
	if err != nil {
		return nil, 0, err
	}

	// v1's normalisation, with one departure: the divisor is how long the two
	// samples were actually apart rather than how long they were asked to be.
	// Under the runner's concurrency the pause routinely overshoots, and dividing
	// by the nominal interval turns that overshoot straight into per cent -- a
	// process that used 210ms of CPU across a 250ms pause would be reported at
	// 105% of a core it never had. The floor of one tick is v1's, and guards what
	// it guards there: a divisor that rounds to zero.
	intervalTicks := max(elapsed.Seconds()*float64(s.clockTicks())*float64(s.cpus()), 1.0)

	out := make([]coreprocs.ProcInfo, 0, len(pids))
	unreadable := 0
	for _, pid := range pids {
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}

		text, err := hostfs.ReadFileLossy(s.fsys(), pidPath(pid, "stat"))
		switch {
		case err != nil && hostfs.IsAbsent(err):
			// Gone between the readdir and the read, which is what a process
			// table does. Not counted; see the note on Unreadable.
			continue
		case err != nil:
			unreadable++
			continue
		}

		comm, ticks, ok := coreprocs.ParseStatCommAndTicks(text)
		if !ok {
			// Readable, and not a stat line. Neither a permission problem nor a
			// race, so it counts: a /proc this cannot parse is something an
			// operator should hear about rather than a quiet gap in the table.
			unreadable++
			continue
		}

		cpuPct := 0.0
		if before, seen := prev[pid]; seen {
			// Clamped at zero, as in v1. A tick counter cannot go backwards, but a
			// pid reused between the two samples looks exactly as though it did,
			// and a negative percentage in the table is worse than a zero.
			cpuPct = max(float64(ticks-before)*100.0/intervalTicks, 0)
		}

		out = append(out, coreprocs.ProcInfo{
			PID:      pid,
			Comm:     comm,
			User:     s.owner(pid),
			RSSBytes: s.rssBytes(pid),
			CPUPct:   cpuPct,
			Cmdline:  s.cmdline(pid),
		})
	}
	return out, unreadable, nil
}

// pids lists the process ids under /proc.
//
// v1's two-step -- isdigit() then int() -- kept as two steps, because they do not
// accept the same strings and internal/pytext exists to preserve the difference.
// Where they disagree v1 raises ValueError and takes the whole snapshot down with
// it; this skips the entry. That is the one departure, and it is unreachable on a
// real /proc: the kernel writes ASCII decimal, and a name int64 cannot hold is not
// a pid either.
func (s Source) pids() ([]int64, error) {
	entries, err := fs.ReadDir(s.fsys(), ".")
	if err != nil {
		return nil, err
	}
	out := make([]int64, 0, len(entries))
	for _, e := range entries {
		if !pytext.IsDigitString(e.Name()) {
			continue
		}
		pid, err := pytext.Int(e.Name())
		if err != nil {
			continue
		}
		out = append(out, pid)
	}
	return out, nil
}

// rssBytes is the resident set, in bytes.
//
// Zero where statm would not read, which is v1's answer and the only honest one
// on offer: a kernel thread genuinely has no resident set, so there is no value
// that could mean "unknown" without also meaning something real.
func (s Source) rssBytes(pid int64) int64 {
	text, err := hostfs.ReadFile(s.fsys(), pidPath(pid, "statm"))
	if err != nil {
		return 0
	}
	pages, ok := coreprocs.ParseStatmRSSPages(text)
	if !ok || pages < 0 {
		return 0
	}
	return pages * s.pageSize()
}

// cmdline is the process's argument vector as one line.
//
// NUL-separated on disk and space-separated here, which is v1's rendering. The
// empty string is the right answer for a kernel thread, which has no argument
// vector at all, and comm is what names those.
func (s Source) cmdline(pid int64) string {
	text, err := hostfs.ReadFileLossy(s.fsys(), pidPath(pid, "cmdline"))
	if err != nil {
		return ""
	}
	line := strings.TrimFunc(strings.ReplaceAll(text, "\x00", " "), pytext.IsSpace)
	return truncate(line, cmdlineCap)
}

// truncate cuts a string to at most n bytes and says that it did.
//
// On a rune boundary, so the result is still text: cutting mid-sequence would put
// bytes in the report that encoding/json replaces with U+FFFD on the way out, and
// a reader would see corruption where the truncation is the whole story.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const marker = "..."
	cut := n - len(marker)
	for cut > 0 && !utf8Start(s[cut]) {
		cut--
	}
	return s[:cut] + marker
}

// utf8Start reports whether b begins a UTF-8 sequence rather than continuing one.
func utf8Start(b byte) bool { return b&0xC0 != 0x80 }

// owner is who the process belongs to, or nil where there is no answer.
func (s Source) owner(pid int64) *string {
	if s.Owner != nil {
		return s.Owner(pid)
	}
	return lookupOwner(s.dir(), pid)
}

func (s Source) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func pidPath(pid int64, name string) string {
	return strconv.FormatInt(pid, 10) + "/" + name
}
