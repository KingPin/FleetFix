// Package procs registers the process-ranking domain's check.
//
// One question: which processes are using this host's memory and CPU. v1 asked
// it in modules/procs/ranker.py and rendered the answer as the Tier 2 Processes
// table; the parsing came across at M2 as internal/core/procs, and what is left
// here is the walk that feeds it and the decision about what the ranking means.
//
// # Why this grades nothing
//
// internal/threshold has no rule for it, and there is no number here that could
// carry one. A process holding forty per cent of RAM is a database doing its job
// on one host and a leak on another, and this check cannot tell them apart --
// system.memory grades how much memory is left, which is the question that has an
// answer. So this domain reports and narrates, and its status is ok whenever the
// walk completed.
//
// The division of labour is worth stating plainly, because it is why both domains
// exist: system.memory and system.load say how bad it is, and procs.top says who
// to look at. One is a verdict and the other is the follow-up question.
//
// # Why one check and not two
//
// Ranking by memory and ranking by CPU look like two checks -- the TUI has a
// toggle between them, and the system domain split five readings apart on exactly
// that reasoning. They are one here because they are one measurement. Both
// rankings come out of a single pass over /proc, and splitting them would mean
// two passes taken at two different moments: an operator who noticed the same pid
// at the top of both lists would be comparing two process tables, and a pid in one
// and not the other could mean anything or nothing. One snapshot, one moment, two
// orderings of it.
//
// It is also the cheaper arrangement, which is the smaller reason but not nothing.
// The CPU percentage needs two samples with an interval between them; folded in,
// the memory ranking rides along on the second sample for free, where two checks
// would pay for three passes and the interval anyway.
//
// # What is deliberately not here
//
// Killing. v1's screen sends SIGTERM and SIGKILL from the same pane, through
// modules/procs/killer.py and two confirm modals. This domain is read-only:
// signalling a process is a Tier 2 action behind the confirm gate, and a check
// that a cron job runs unattended must not be able to do it.
package procs

import (
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
)

// TopID is the domain's check id. Public API: it appears in checks[], in --check
// selectors, and in whatever Ansible an operator writes against it.
const TopID check.ID = "procs.top"

// Metric names, likewise public.
//
// Unlabelled, and that is the decision worth explaining. Labelling the leaders by
// process name would give a dashboard the name for free, and would also mint a new
// Prometheus series every time the busiest process changed -- which on a working
// host is every scrape. disk.usage labels by mount because a mount is still there
// tomorrow; a comm is not. So these are the numbers, the report is where the names
// are, and an alert that fires on procs.top_rss_bytes reads data[] to find out who.
const (
	CountMetric      = "procs.count"
	UnreadableMetric = "procs.unreadable"
	TopRSSMetric     = "procs.top_rss_bytes"
	TopCPUMetric     = "procs.top_cpu_pct"
)

// TopParam names the parameter, public for the reason the id is: an operator types
// it after --param.
const TopParam = "top"

// DefaultTop is how many processes each ranking reports.
//
// v1 has two answers -- ranker.top_by_rss defaults to 10 and the screen asks for
// 25 -- and this takes the library's rather than the screen's. Twenty-five rows
// is a table an operator scrolls; ten is a list somebody reads in a ticket, and
// this document ends up in tickets. The parameter is there for anyone who wants
// v1's screenful back.
const DefaultTop = 10

// DefaultSampleInterval is v1's sample_interval_s (modules/procs/ranker.py:96).
//
// Short, and deliberately so on v1's part: it is the pause between two reads of
// every /proc/<pid>/stat, and the TUI blocked on it every refresh. It stays short
// here for a different reason -- a one-shot report should not spend a second
// standing still -- with the honest consequence that the percentage is a coarse
// reading. A process that woke up once inside a fifth of a second can look busy,
// and the ranking is the useful part rather than the digits.
const DefaultSampleInterval = 200 * time.Millisecond

// DefaultClockTicks is USER_HZ: the unit /proc/<pid>/stat reports CPU time in.
//
// v1 asks sysconf(SC_CLK_TCK) and falls back to 100. Go has no sysconf, and does
// not need one here: USER_HZ is fixed at 100 on Linux regardless of the kernel's
// own CONFIG_HZ -- the kernel converts on the way out of /proc precisely so that
// this number is not a host property -- and glibc's sysconf returns the same
// constant rather than asking anything. A field on Source exists anyway, because a
// test that fixes the arithmetic should not have to reason about a constant.
const DefaultClockTicks int64 = 100

// budget caps the walk. Two passes over every /proc entry plus the interval
// between them; a thousand processes is an ordinary number and ten thousand is a
// container host, so this is sized for the second.
const budget = 20 * time.Second

// stepCap bounds how many processes get a line of their own, per ranking.
//
// Five rather than the ten the other domains settled on, because this check
// narrates two lists and twenty lines would bury the summary between them. The
// remainder is stated rather than dropped quietly, and data[] carries the full
// top-N of both regardless.
const stepCap = 5

// cmdlineCap bounds the command line carried in data[].
//
// A wire decision, not v1's display one: v1 truncates to 80 columns because that
// is what fits a table cell, and the full string stays in memory behind it. Here
// the string is the wire, and a JVM's classpath or a container runtime's argument
// vector runs to kilobytes -- one process would then outweigh the rest of the
// report, in a field documented as best-effort detail. Truncation is marked, so a
// reader can see that there was more.
const cmdlineCap = 512

// DefaultProcDir is where /proc is mounted.
const DefaultProcDir = "/proc"

// A Source is where this domain reads from. The zero value is the live host.
//
// Dir and FS are both here, and neither derives from the other, for logsqueeze's
// reason turned around: fs.FS gives no way to ask who owns a file -- fs.FileInfo's
// Sys is nil under fstest.MapFS and platform-specific everywhere else -- so the
// owner lookup needs a real path, while the reads want a filesystem a test can
// stage without a /proc of its own.
type Source struct {
	// Dir is where /proc is mounted, used by the owner lookup. Empty means
	// DefaultProcDir.
	Dir string

	// FS reads the process tree. Nil means the live filesystem rooted at Dir.
	FS fs.FS

	// PageSize turns statm's page count into bytes. Zero means this host's, which
	// is what v1's sysconf(SC_PAGE_SIZE) asks for.
	PageSize int64

	// ClockTicks is USER_HZ. Zero means DefaultClockTicks.
	ClockTicks int64

	// CPUs is what the CPU percentage is normalised by, so that a process pinning
	// one core of eight reads as 12.5% rather than 100%. Nil means this host's
	// count. v1 divides by os.cpu_count() for the same reason, and with the same
	// consequence: the column does not sum to 100.
	CPUs func() int

	// SampleInterval is the pause between the two stat reads. Zero means
	// DefaultSampleInterval; a test sets it to something it does not have to wait
	// for.
	SampleInterval time.Duration

	// Now is the clock the sample interval is measured with. Nil means time.Now.
	// A seam because the percentage is divided by what this reports, so a test
	// that asserts arithmetic should not also be asserting that a timer fired
	// when it said it would.
	Now func() time.Time

	// Owner answers who owns a process, by pid. Nil means the live lookup: stat
	// <Dir>/<pid>/status for the uid, then this host's user database for the name.
	//
	// The result is nil where there is no answer, which is an ordinary outcome
	// rather than a failure -- a uid with no passwd entry is the normal state of
	// affairs inside a container, and the process is real either way. Reporting
	// the absence beats inventing "root" or printing the number twice.
	Owner func(pid int64) *string
}

// New returns a Source reading the live /proc.
func New() Source { return Source{Dir: DefaultProcDir} }

func (s Source) dir() string {
	if s.Dir == "" {
		return DefaultProcDir
	}
	return s.Dir
}

func (s Source) fsys() fs.FS {
	if s.FS != nil {
		return s.FS
	}
	return os.DirFS(s.dir())
}

func (s Source) pageSize() int64 {
	if s.PageSize > 0 {
		return s.PageSize
	}
	return int64(os.Getpagesize())
}

func (s Source) clockTicks() int64 {
	if s.ClockTicks > 0 {
		return s.ClockTicks
	}
	return DefaultClockTicks
}

// cpus is the divisor, floored at one. A zero would divide the percentage by
// nothing and a negative would invert its sign, and neither is a number any
// caller means by "how many CPUs does this host have".
func (s Source) cpus() int {
	n := runtime.NumCPU()
	if s.CPUs != nil {
		n = s.CPUs()
	}
	if n < 1 {
		return 1
	}
	return n
}

func (s Source) sampleInterval() time.Duration {
	if s.SampleInterval > 0 {
		return s.SampleInterval
	}
	return DefaultSampleInterval
}

// Checks returns the domain's checks.
//
// A constructor rather than registration by init(), for the reason builtin.Checks
// gives: what a build runs must not depend on what the linker kept.
func Checks(src Source) []check.Check {
	return []check.Check{top{src: src}}
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

// count renders a number with its noun, both forms spelled out.
//
// The other domains derive the plural by appending an s, which is right for
// "path", "file" and "key" and wrong for the only noun this one has -- "process"
// with an s on the end is not a word, and every rule that fixes it breaks
// something else.
func count(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// processes is the noun this domain counts, which is every counted thing in it.
func processes(n int) string { return count(n, "process", "processes") }

// humanBytes renders a byte count for a summary line. Binary steps, one decimal
// past the first unit -- the same rendering the docker, system, logsqueeze and
// storage domains use, so two lines of one report do not disagree about what a
// gigabyte is.
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
