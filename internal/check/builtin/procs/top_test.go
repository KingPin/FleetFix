package procs

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coreprocs "github.com/KingPin/FleetFix/v2/internal/core/procs"
)

// A host with something obvious on it: one big process, one busy one, and one
// that is neither.
//
// Two staged filesystems rather than one, because a CPU percentage is a rate and
// a single filesystem would answer both samples with the same tick counter --
// every process would then read as idle, which is a fine thing to test and not
// this. The first pass sees the counters at zero and the second sees the work,
// so the ticks below are the work done during the interval.
func busyHost() fs.FS {
	idle := stage(
		proc{pid: 1, comm: "systemd", pages: 1000, cmdline: "/sbin/init"},
		proc{pid: 42, comm: "postgres", pages: 100000, cmdline: "postgres -D /var/lib/pg"},
		proc{pid: 99, comm: "rustc", pages: 5000, cmdline: "rustc --edition 2021 lib.rs"},
	)
	busy := stage(
		proc{pid: 1, comm: "systemd", ticks: 100, pages: 1000, cmdline: "/sbin/init"},
		proc{pid: 42, comm: "postgres", ticks: 100, pages: 100000, cmdline: "postgres -D /var/lib/pg"},
		proc{pid: 99, comm: "rustc", ticks: 300, pages: 5000, cmdline: "rustc --edition 2021 lib.rs"},
	)
	return &swapFS{first: idle, second: busy, at: 3}
}

func TestTheRankingsComeOutOfOneSnapshot(t *testing.T) {
	res, _ := run(t, source(busyHost()), nil)

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s (%s)", res.Status, res.Summary)
	}
	rep := report(t, res)
	if rep.Total != 3 {
		t.Errorf("total = %d, want 3", rep.Total)
	}
	if got, want := len(rep.ByRSS), len(rep.ByCPU); got != want {
		t.Errorf("the two rankings hold %d and %d rows; they are one set of processes", got, want)
	}
	if got := comms(rep.ByRSS); got[0] != "postgres" {
		t.Errorf("by_rss = %v, want postgres first", got)
	}
	if got := comms(rep.ByCPU); got[0] != "rustc" {
		t.Errorf("by_cpu = %v, want rustc first", got)
	}
}

// The first sample saw every counter at zero, so the whole tick count arrives as
// new work. The check is the arithmetic, not the ordering: with the clock fixed
// at a second, one core and 100 ticks a second, a process that burned 300 ticks
// has used three seconds of CPU in one, and reads as 300%.
func TestThePercentageIsTicksOverTheMeasuredInterval(t *testing.T) {
	res, _ := run(t, source(busyHost()), nil)
	rep := report(t, res)

	byPID := map[int64]float64{}
	for _, p := range rep.ByCPU {
		byPID[p.PID] = p.CPUPct
	}
	for pid, want := range map[int64]float64{1: 100, 42: 100, 99: 300} {
		if got := byPID[pid]; got != want {
			t.Errorf("pid %d cpu = %.1f%%, want %.1f%%", pid, got, want)
		}
	}
}

// Per core, which is v1's normalisation and the reason the column does not sum to
// 100: the same three seconds of CPU on a four-core host is a quarter of the box.
func TestThePercentageIsNormalisedByCPUCount(t *testing.T) {
	src := source(busyHost())
	src.CPUs = func() int { return 4 }
	rep := report(t, mustRun(t, src, nil))

	if got := rep.ByCPU[0].CPUPct; got != 75 {
		t.Errorf("busiest = %.1f%%, want 75%% on four cores", got)
	}
}

// The first sample is what the second is measured against, so a process that used
// nothing between them reads as idle no matter how much CPU it has used since boot.
func TestAProcessThatDidNothingBetweenSamplesIsIdle(t *testing.T) {
	fsys := stage(proc{pid: 7, comm: "sleeper", ticks: 900000, pages: 10})
	rep := report(t, mustRun(t, source(fsys), nil))

	if got := rep.ByCPU[0].CPUPct; got != 0 {
		t.Errorf("cpu = %.1f%%, want 0 for a process whose counter did not move", got)
	}
}

// A pid reused between the two samples looks like a counter running backwards.
// v1 clamps, and so does this: a negative percentage in the table is worse than a
// zero.
func TestACounterThatWentBackwardsReadsAsIdle(t *testing.T) {
	first := stage(proc{pid: 7, comm: "gone", ticks: 5000, pages: 10})
	second := stage(proc{pid: 7, comm: "new", ticks: 3, pages: 10})

	rep := report(t, mustRun(t, source(&swapFS{first: first, second: second, at: 1}), nil))
	if got := rep.ByCPU[0].CPUPct; got != 0 {
		t.Errorf("cpu = %.1f%%, want 0", got)
	}
}

// A process that appeared during the interval has nothing to subtract from, and
// is reported at zero rather than dropped -- the second pass is what decides who
// exists.
func TestAProcessThatStartedDuringTheIntervalIsStillReported(t *testing.T) {
	first := stage(proc{pid: 1, comm: "systemd", ticks: 10, pages: 100})
	second := stage(
		proc{pid: 1, comm: "systemd", ticks: 10, pages: 100},
		proc{pid: 2, comm: "newcomer", ticks: 400, pages: 100},
	)

	rep := report(t, mustRun(t, source(&swapFS{first: first, second: second, at: 1}), nil))
	if rep.Total != 2 {
		t.Fatalf("total = %d, want 2", rep.Total)
	}
	for _, p := range rep.ByCPU {
		if p.PID == 2 && p.CPUPct != 0 {
			t.Errorf("newcomer cpu = %.1f%%, want 0: there was nothing to measure it against", p.CPUPct)
		}
	}
}

func TestTheResidentSetIsPagesTimesThePageSize(t *testing.T) {
	fsys := stage(proc{pid: 5, comm: "app", pages: 250})
	rep := report(t, mustRun(t, source(fsys), nil))

	if got := rep.ByRSS[0].RSSBytes; got != 250*pageSize {
		t.Errorf("rss = %d, want %d", got, 250*pageSize)
	}
}

// v1's answer, and the only honest one: a kernel thread has no resident set, so
// there is no value that could mean "unknown" without also meaning something real.
func TestAProcessWithNoStatmIsReportedAtZero(t *testing.T) {
	fsys := stage(proc{pid: 5, comm: "kthreadd", noStatm: true})
	rep := report(t, mustRun(t, source(fsys), nil))

	if rep.Total != 1 {
		t.Fatalf("total = %d, want the process reported anyway", rep.Total)
	}
	if got := rep.ByRSS[0].RSSBytes; got != 0 {
		t.Errorf("rss = %d, want 0", got)
	}
}

// A statm that reads and does not parse is the same answer as no statm at all:
// there is no resident set to report, and the process is still real.
func TestAStatmThatMakesNoSenseIsReportedAtZero(t *testing.T) {
	for name, body := range map[string]string{
		"unparseable":  "not a statm line\n",
		"one field":    "512\n",
		"negative rss": "0 -5 0 0 0 0 0\n",
	} {
		t.Run(name, func(t *testing.T) {
			fsys := stage(proc{pid: 5, comm: "app", pages: 1})
			fsys["5/statm"] = &fstest.MapFile{Data: []byte(body)}

			rep := report(t, mustRun(t, source(fsys), nil))
			if got := rep.ByRSS[0].RSSBytes; got != 0 {
				t.Errorf("rss = %d, want 0", got)
			}
		})
	}
}

// The one place this departs from v1, which raises ValueError on the whole
// snapshot: a name made of digits that no integer can hold is not a pid, and the
// kernel does not write one. Skipped rather than fatal.
func TestADigitStringTooWideForAPIDIsNotOne(t *testing.T) {
	fsys := stage(proc{pid: 5, comm: "app", pages: 1})
	fsys[strings.Repeat("9", 40)+"/stat"] = &fstest.MapFile{Data: []byte("nonsense\n")}

	rep := report(t, mustRun(t, source(fsys), nil))
	if rep.Total != 1 {
		t.Errorf("total = %d, want 1", rep.Total)
	}
	if rep.Unreadable != 0 {
		t.Errorf("unreadable = %d, want 0: it was never a process", rep.Unreadable)
	}
}

func TestTheCommandLineIsSpaceSeparated(t *testing.T) {
	fsys := stage(proc{pid: 5, comm: "pg", pages: 1, cmdline: "postgres -D /var/lib/pg"})
	rep := report(t, mustRun(t, source(fsys), nil))

	if got := rep.ByRSS[0].Cmdline; got != "postgres -D /var/lib/pg" {
		t.Errorf("cmdline = %q", got)
	}
}

// A kernel thread has no argument vector at all, and comm is what names it.
func TestAProcessWithNoCommandLineGetsAnEmptyOne(t *testing.T) {
	fsys := stage(proc{pid: 5, comm: "kworker", noCmdline: true})
	rep := report(t, mustRun(t, source(fsys), nil))

	if got := rep.ByRSS[0].Cmdline; got != "" {
		t.Errorf("cmdline = %q, want empty", got)
	}
	if got := rep.ByRSS[0].Comm; got != "kworker" {
		t.Errorf("comm = %q", got)
	}
}

// One process must not outweigh the rest of the report.
func TestALongCommandLineIsTruncatedOnTheWire(t *testing.T) {
	long := strings.Repeat("a", 4000)
	fsys := stage(proc{pid: 5, comm: "java", pages: 1, cmdline: long})
	rep := report(t, mustRun(t, source(fsys), nil))

	got := rep.ByRSS[0].Cmdline
	if len(got) > cmdlineCap {
		t.Errorf("cmdline is %d bytes, want at most %d", len(got), cmdlineCap)
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("cmdline = %q, want the truncation marked", got[max(0, len(got)-20):])
	}
}

// Byte-stability: two processes holding the same amount, ordered by something
// that does not change between runs.
func TestEqualRowsAreOrderedByPID(t *testing.T) {
	fsys := stage(
		proc{pid: 30, comm: "c", pages: 100, ticks: 5},
		proc{pid: 10, comm: "a", pages: 100, ticks: 5},
		proc{pid: 20, comm: "b", pages: 100, ticks: 5},
	)
	rep := report(t, mustRun(t, source(fsys), nil))

	for _, rows := range [][]int64{pids(rep.ByRSS), pids(rep.ByCPU)} {
		if got := rows; got[0] != 10 || got[1] != 20 || got[2] != 30 {
			t.Errorf("order = %v, want ascending pid", got)
		}
	}
}

func TestOnlyNumericEntriesAreProcesses(t *testing.T) {
	fsys := stage(proc{pid: 5, comm: "app", pages: 1})
	// The rest of what /proc actually holds.
	for _, name := range []string{"meminfo", "self/stat", "net/dev", "sys/kernel/hostname"} {
		fsys[name] = &fstest.MapFile{Data: []byte("not a process\n")}
	}

	rep := report(t, mustRun(t, source(fsys), nil))
	if rep.Total != 1 {
		t.Errorf("total = %d, want 1: only the numeric entries are processes", rep.Total)
	}
}

// A process that exited between the readdir and the read is what a process table
// does, and counting them would report how busy the host is at forking under a
// heading about permissions.
func TestAProcessThatVanishedMidWalkIsNotCounted(t *testing.T) {
	first := stage(
		proc{pid: 1, comm: "systemd", ticks: 10, pages: 100},
		proc{pid: 2, comm: "shortlived", ticks: 10, pages: 100},
	)
	second := stage(proc{pid: 1, comm: "systemd", ticks: 10, pages: 100})
	// The directory entry survives the readdir and the file underneath it does
	// not, which is the race as it actually happens.
	second["2"] = &fstest.MapFile{Mode: fs.ModeDir}

	res := mustRun(t, source(&swapFS{first: first, second: second, at: 2}), nil)
	rep := report(t, res)

	if rep.Unreadable != 0 {
		t.Errorf("unreadable = %d, want 0: an exiting process is not a denial", rep.Unreadable)
	}
	if strings.Contains(res.Summary, "could not be read") {
		t.Errorf("summary = %q, want no incompleteness note", res.Summary)
	}
}

// The opposite case: a process that is there and will not be read is exactly the
// one an operator hunting a memory hog is looking for.
func TestAnUnreadableProcessIsCountedAndSaid(t *testing.T) {
	fsys := stage(proc{pid: 1, comm: "systemd", ticks: 10, pages: 100})
	fsys["2/stat"] = &fstest.MapFile{Data: []byte("x"), Mode: 0}

	res, events := mustRunSteps(t, source(deniedFS{MapFS: fsys, deny: "2/stat"}), nil)
	rep := report(t, res)

	if rep.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", rep.Unreadable)
	}
	if !strings.Contains(res.Summary, "1 process could not be read") {
		t.Errorf("summary = %q, want it to say the rankings are partial", res.Summary)
	}
	if !hasWarnStep(events, "could not be read") {
		t.Error("no warn step said the rankings are partial")
	}
}

// A /proc entry that reads and does not parse is neither a race nor a permission
// problem, so it counts.
func TestAStatFileThatDoesNotParseIsCounted(t *testing.T) {
	fsys := stage(
		proc{pid: 1, comm: "systemd", ticks: 10, pages: 100},
		proc{pid: 2, comm: "junk", badStat: true},
	)
	rep := report(t, mustRun(t, source(fsys), nil))

	if rep.Unreadable != 1 {
		t.Errorf("unreadable = %d, want 1", rep.Unreadable)
	}
	if rep.Total != 1 {
		t.Errorf("total = %d, want 1", rep.Total)
	}
}

func TestTheRankingDepthBoundsBothLists(t *testing.T) {
	var staged []proc
	for i := int64(1); i <= 30; i++ {
		staged = append(staged, proc{pid: i, comm: "p", pages: i, ticks: i})
	}
	rep := report(t, mustRun(t, source(stage(staged...)), map[string]string{TopParam: "3"}))

	if len(rep.ByRSS) != 3 || len(rep.ByCPU) != 3 {
		t.Errorf("lists are %d and %d long, want 3", len(rep.ByRSS), len(rep.ByCPU))
	}
	if rep.Total != 30 {
		t.Errorf("total = %d, want every process counted regardless of the depth", rep.Total)
	}
}

func TestADepthLargerThanTheHostIsNotAnError(t *testing.T) {
	rep := report(t, mustRun(t, source(busyHost()), map[string]string{TopParam: "500"}))
	if len(rep.ByRSS) != 3 {
		t.Errorf("by_rss holds %d rows, want the 3 that exist", len(rep.ByRSS))
	}
}

func TestADepthThatIsNotAPositiveNumberIsAnError(t *testing.T) {
	for _, raw := range []string{"lots", "0", "-1", "3.5"} {
		t.Run(raw, func(t *testing.T) {
			res, _ := run(t, source(busyHost()), map[string]string{TopParam: raw})
			if res.Status != check.StatusError {
				t.Errorf("status = %s, want error", res.Status)
			}
			if !strings.Contains(res.Summary, TopParam) {
				t.Errorf("summary = %q, want it to name the parameter", res.Summary)
			}
		})
	}
}

// The runner applies the spec default, so an empty value only reaches Run when a
// caller built the Input itself -- and the check has to mean the same thing then.
func TestAnAbsentDepthMeansTheDefault(t *testing.T) {
	var staged []proc
	for i := int64(1); i <= 20; i++ {
		staged = append(staged, proc{pid: i, comm: "p", pages: i, ticks: i})
	}
	rep := report(t, mustRun(t, source(stage(staged...)), nil))

	if len(rep.ByRSS) != DefaultTop {
		t.Errorf("by_rss holds %d rows, want %d", len(rep.ByRSS), DefaultTop)
	}
}

func TestTheNarrationIsCappedPerRanking(t *testing.T) {
	var staged []proc
	for i := int64(1); i <= 20; i++ {
		staged = append(staged, proc{pid: i, comm: "p", pages: i, ticks: i})
	}
	_, events := run(t, source(stage(staged...)), nil)

	lines := texts(events)
	remainders := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "and ") {
			remainders++
		}
	}
	// Five rows plus one remainder line, twice.
	if len(lines) != 2*(stepCap+1) {
		t.Errorf("emitted %d steps, want %d:\n%s", len(lines), 2*(stepCap+1), strings.Join(lines, "\n"))
	}
	if remainders != 2 {
		t.Errorf("got %d remainder lines, want one per ranking", remainders)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "5 processes further down") {
		t.Errorf("remainder does not state the count:\n%s", strings.Join(lines, "\n"))
	}
}

func TestTheNarrationNamesTheOwner(t *testing.T) {
	src := source(busyHost())
	name := "postgres"
	src.Owner = func(pid int64) *string {
		if pid == 42 {
			return &name
		}
		return nil
	}
	_, events := run(t, src, nil)

	joined := strings.Join(texts(events), "\n")
	if !strings.Contains(joined, "run by postgres") {
		t.Errorf("no line named the owner:\n%s", joined)
	}
	if !strings.Contains(joined, "no passwd entry for its uid") {
		t.Errorf("a process with no owner was not described as such:\n%s", joined)
	}
}

func TestTheMetricsCarryTheLeaders(t *testing.T) {
	res := mustRun(t, source(busyHost()), nil)

	if got := metricNamed(t, res, CountMetric).Value; got != 3 {
		t.Errorf("%s = %v, want 3", CountMetric, got)
	}
	if got := metricNamed(t, res, UnreadableMetric).Value; got != 0 {
		t.Errorf("%s = %v, want 0", UnreadableMetric, got)
	}
	if got := metricNamed(t, res, TopRSSMetric).Value; got != float64(100000*pageSize) {
		t.Errorf("%s = %v", TopRSSMetric, got)
	}
	if got := metricNamed(t, res, TopCPUMetric).Value; got != 300 {
		t.Errorf("%s = %v, want 300", TopCPUMetric, got)
	}
	// Unlabelled on purpose: a comm as a label mints a new series every scrape.
	for _, m := range res.Metrics {
		if len(m.Labels) != 0 {
			t.Errorf("%s carries labels %v", m.Name, m.Labels)
		}
	}
}

// /proc always holds at least the process doing the reading, so nothing here
// means this is not a process table.
func TestAProcWithNoProcessesIsUnavailable(t *testing.T) {
	res, _ := run(t, source(stage()), nil)

	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %s, want unavailable (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "looks like a process") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestAMissingProcIsUnavailable(t *testing.T) {
	res, _ := run(t, source(erroringFS{err: fs.ErrNotExist}), nil)

	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %s, want unavailable (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "has no /proc") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestAnUnlistableProcIsUnavailable(t *testing.T) {
	res, _ := run(t, source(erroringFS{err: fs.ErrPermission}), nil)

	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %s, want unavailable (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "not listable") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestAProcThatFailsForSomeOtherReasonIsAnError(t *testing.T) {
	res, _ := run(t, source(erroringFS{err: errors.New("i/o error")}), nil)

	if res.Status != check.StatusError {
		t.Errorf("status = %s, want error (%s)", res.Status, res.Summary)
	}
}

// The budget cuts off the result, not the goroutine, so the walk has to honour
// the context itself or the run outlives its own report.
func TestACancelledRunStopsAndSaysSo(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := top{src: source(busyHost())}.Run(ctx, check.Input{Params: map[string]string{}, Progress: check.Discard})
	if res.Status != check.StatusError {
		t.Errorf("status = %s, want error (%s)", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "did not finish in time") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// The pause is a timer rather than a sleep for the same reason, and a run
// cancelled while it is waiting must return then rather than at the end of it.
func TestTheSampleIntervalIsCancellable(t *testing.T) {
	src := source(busyHost())
	src.SampleInterval = time.Hour

	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	done := make(chan check.Result, 1)
	go func() {
		done <- top{src: src}.Run(ctx, check.Input{Params: map[string]string{}, Progress: check.Discard})
	}()

	select {
	case res := <-done:
		if res.Status != check.StatusError {
			t.Errorf("status = %s, want error", res.Status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the check waited out its sample interval after the run was cancelled")
	}
}

func TestTheSampledIntervalIsReported(t *testing.T) {
	rep := report(t, mustRun(t, source(busyHost()), nil))
	if rep.SampleMS != 1000 {
		t.Errorf("sample_ms = %d, want the second the clock advanced by", rep.SampleMS)
	}
}

// Nothing on the wire may be null where the schema promises a list.
//
// Result.Normalize does not reach into data[], so a nil slice here would reach a
// consumer as a null that no other list in the document can be. The one null this
// report may carry is a row's user, which is the documented answer for a uid with
// no passwd entry rather than a missing list.
func TestTheReportNeverHoldsANullList(t *testing.T) {
	res := mustRun(t, source(busyHost()), nil)
	b := string(mustMarshal(t, res.Data))

	for _, field := range []string{"by_rss", "by_cpu"} {
		if strings.Contains(b, `"`+field+`":null`) {
			t.Errorf("%s is null on the wire: %s", field, b)
		}
	}
}

// The shipped check reads the live clock, which the staged Source hides.
func TestTheShippedSourceUsesTheLiveClock(t *testing.T) {
	src := New()
	if src.Now != nil {
		t.Error("New pinned a clock; the live host's is time.Now")
	}
	before := time.Now()
	if got := src.now(); got.Before(before) {
		t.Errorf("now() = %v, want the live clock", got)
	}
}

func mustRun(t *testing.T, src Source, params map[string]string) check.Result {
	t.Helper()
	res, _ := mustRunSteps(t, src, params)
	return res
}

// mustRunSteps is mustRun for a test that also reads what was narrated. The
// events come from the recorder rather than from Result.Steps, which the runner
// fills in from the same emitter after Run has returned.
func mustRunSteps(t *testing.T, src Source, params map[string]string) (check.Result, []check.Event) {
	t.Helper()
	res, events := run(t, src, params)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %s (%s)", res.Status, res.Summary)
	}
	return res, events
}

func pids(rows []coreprocs.ProcInfo) []int64 {
	out := make([]int64, len(rows))
	for i, r := range rows {
		out[i] = r.PID
	}
	return out
}

func hasWarnStep(events []check.Event, substr string) bool {
	for _, e := range events {
		if e.Status == check.StatusWarn && strings.Contains(e.Text, substr) {
			return true
		}
	}
	return false
}

// swapFS serves one filesystem for the first `at` opens of a stat file and
// another afterwards, so a test can stage what changed between the two samples.
type swapFS struct {
	first, second fstest.MapFS
	at            int32

	seen atomic.Int32
}

func (s *swapFS) Open(name string) (fs.File, error) {
	if strings.HasSuffix(name, "/stat") && s.seen.Add(1) > s.at {
		return s.second.Open(name)
	}
	if strings.HasSuffix(name, "/stat") {
		return s.first.Open(name)
	}
	return s.second.Open(name)
}

func (s *swapFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if s.seen.Load() >= s.at {
		return s.second.ReadDir(name)
	}
	return s.first.ReadDir(name)
}

// deniedFS refuses one path and serves the rest.
type deniedFS struct {
	fstest.MapFS
	deny string
}

func (d deniedFS) Open(name string) (fs.File, error) {
	if name == d.deny {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrPermission}
	}
	return d.MapFS.Open(name)
}

// erroringFS fails the directory listing itself.
type erroringFS struct{ err error }

func (e erroringFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: e.err}
}
