package procs

import (
	"encoding/json"
	"io/fs"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coreprocs "github.com/KingPin/FleetFix/v2/internal/core/procs"
)

// A proc is one staged process: what its stat, statm and cmdline say.
//
// The fixture is a synthetic /proc rather than a captured one because the check
// is a walk, not a parser -- internal/core/procs already carries the parsers and
// their differential corpus, and what is under test here is which entries the
// walk keeps, what it does with the ones it cannot read, and how it ranks the
// rest.
type proc struct {
	pid     int64
	comm    string
	ticks   int64 // utime + stime, split evenly across the two fields
	pages   int64 // statm's resident field
	cmdline string

	noStatm   bool // statm missing, as for a process that exited between reads
	noCmdline bool
	badStat   bool // present and not a stat line
}

// statLine renders /proc/<pid>/stat with the process's name where the kernel puts
// it and its ticks in fields 14 and 15.
func (p proc) statLine() string {
	if p.badStat {
		return "this is not a stat line\n"
	}
	// The parser splits after the last ')', so rest[0] is field 3 -- the state --
	// and the tick pair is fields 14 and 15, which is rest[11] and rest[12]. Ten
	// placeholders sit between the state and the pair.
	fields := make([]string, 0, 30)
	fields = append(fields, "S")
	for i := 0; i < 10; i++ {
		fields = append(fields, "0")
	}
	utime := p.ticks / 2
	fields = append(fields, strconv.FormatInt(utime, 10), strconv.FormatInt(p.ticks-utime, 10))
	for i := 0; i < 8; i++ {
		fields = append(fields, "0")
	}
	return strconv.FormatInt(p.pid, 10) + " (" + p.comm + ") " + strings.Join(fields, " ") + "\n"
}

// stage builds a /proc holding exactly these processes.
func stage(procs ...proc) fstest.MapFS {
	fsys := fstest.MapFS{}
	for _, p := range procs {
		dir := strconv.FormatInt(p.pid, 10)
		fsys[dir+"/stat"] = &fstest.MapFile{Data: []byte(p.statLine())}
		if !p.noStatm {
			fsys[dir+"/statm"] = &fstest.MapFile{
				Data: []byte("0 " + strconv.FormatInt(p.pages, 10) + " 0 0 0 0 0\n"),
			}
		}
		if !p.noCmdline {
			fsys[dir+"/cmdline"] = &fstest.MapFile{
				Data: []byte(strings.ReplaceAll(p.cmdline, " ", "\x00") + "\x00"),
			}
		}
	}
	return fsys
}

// pageSize is the fixture's page size: a round number so the arithmetic in a
// failure message is readable, rather than this host's.
const pageSize = 4096

// source is a Source over a staged /proc, with every host property fixed.
func source(fsys fs.FS) Source {
	return Source{
		Dir:            "/proc",
		FS:             fsys,
		PageSize:       pageSize,
		ClockTicks:     100,
		CPUs:           func() int { return 1 },
		SampleInterval: time.Millisecond,
		Owner:          func(int64) *string { return nil },
		Now:            steadyClock(time.Second),
	}
}

// steadyClock advances by step on every reading, so the measured interval is a
// constant the test chose rather than however long a timer took to fire.
func steadyClock(step time.Duration) func() time.Time {
	at := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	return func() time.Time {
		now := at
		at = at.Add(step)
		return now
	}
}

type recorder struct{ events []check.Event }

func (r *recorder) Emit(e check.Event) { r.events = append(r.events, e) }

// run executes the check over a source, with the given parameters.
func run(t *testing.T, src Source, params map[string]string) (check.Result, []check.Event) {
	t.Helper()
	rec := &recorder{}
	in := check.Input{Params: params, Progress: rec}
	if in.Params == nil {
		in.Params = map[string]string{}
	}
	res := top{src: src}.Run(t.Context(), in)
	return res, rec.events
}

// report decodes data[] back out through JSON, so a test asserts what a consumer
// receives rather than what the struct happened to hold.
func report(t *testing.T, res check.Result) Report {
	t.Helper()
	b, err := json.Marshal(res.Data)
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}
	var out Report
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	return out
}

func comms(rows []coreprocs.ProcInfo) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Comm
	}
	return out
}

func texts(events []check.Event) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Text
	}
	return out
}

func metricNamed(t *testing.T, res check.Result, name string) check.Metric {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no metric %q in %v", name, res.Metrics)
	return check.Metric{}
}

func TestTheDomainRegistersOneCheck(t *testing.T) {
	got := Checks(New())
	if len(got) != 1 {
		t.Fatalf("got %d checks, want 1", len(got))
	}
	if id := got[0].Spec().ID; id != TopID {
		t.Errorf("id = %q, want %q", id, TopID)
	}
	if err := got[0].Spec().Validate(); err != nil {
		t.Errorf("spec is malformed: %v", err)
	}
}

// The parameter has a default, so the runner never has to skip this check for
// want of it -- which is what keeps a domain with a parameter in the default set.
func TestTheRankingDepthIsOptional(t *testing.T) {
	spec := top{}.Spec()
	if !spec.InDefault {
		t.Error("procs.top is out of the default set")
	}
	if len(spec.Params) != 1 {
		t.Fatalf("got %d params, want 1", len(spec.Params))
	}
	p := spec.Params[0]
	if p.Name != TopParam || p.Required || p.Default != "10" {
		t.Errorf("param = %+v, want an optional %q defaulting to 10", p, TopParam)
	}
}

// The zero Source is the live host, so a caller that only wants to know what this
// build can check costs nothing.
func TestTheZeroSourceIsTheLiveHost(t *testing.T) {
	var zero Source
	if got := zero.dir(); got != DefaultProcDir {
		t.Errorf("dir = %q, want %q", got, DefaultProcDir)
	}
	if zero.pageSize() <= 0 {
		t.Errorf("pageSize = %d, want this host's", zero.pageSize())
	}
	if got := zero.clockTicks(); got != DefaultClockTicks {
		t.Errorf("clockTicks = %d, want %d", got, DefaultClockTicks)
	}
	if got := zero.sampleInterval(); got != DefaultSampleInterval {
		t.Errorf("sampleInterval = %v, want %v", got, DefaultSampleInterval)
	}
	if zero.cpus() < 1 {
		t.Errorf("cpus = %d, want at least one", zero.cpus())
	}
	if zero.now().IsZero() {
		t.Error("now returned the zero time, so the clock is not defaulted")
	}
}

// A divisor of zero would send every percentage to infinity and a negative one
// would invert its sign, and neither is what a caller means by a CPU count.
func TestTheCPUCountIsFlooredAtOne(t *testing.T) {
	for _, n := range []int{0, -4} {
		src := Source{CPUs: func() int { return n }}
		if got := src.cpus(); got != 1 {
			t.Errorf("cpus() with %d = %d, want 1", n, got)
		}
	}
}

func TestHumanBytesRoundsPastTheFirstUnit(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want string
	}{{0, "0 B"}, {1023, "1023 B"}, {1024, "1.0 KB"}, {1536, "1.5 KB"}, {1 << 30, "1.0 GB"}} {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestTheNounIsSpelledOutInBothNumbers(t *testing.T) {
	if got := processes(1); got != "1 process" {
		t.Errorf("processes(1) = %q", got)
	}
	if got := processes(3); got != "3 processes" {
		t.Errorf("processes(3) = %q", got)
	}
	if got := processes(0); got != "0 processes" {
		t.Errorf("processes(0) = %q", got)
	}
}

func TestTruncateCutsOnARuneBoundary(t *testing.T) {
	// Four-byte runes, so a naive cut at any of the caps below would land mid
	// sequence and put an invalid byte on the wire.
	s := strings.Repeat("🙂", 10)
	got := truncate(s, 20)
	if len(got) > 20 {
		t.Errorf("truncate produced %d bytes, want at most 20", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncate = %q, want it marked", got)
	}
	if !json.Valid(mustMarshal(t, strings.TrimSuffix(got, "..."))) {
		t.Errorf("truncate = %q, which is not valid text", got)
	}
	for _, r := range strings.TrimSuffix(got, "...") {
		if r == '�' {
			t.Fatalf("truncate cut mid-rune: %q", got)
		}
	}
}

func TestTruncateLeavesAShortStringAlone(t *testing.T) {
	if got := truncate("short", 512); got != "short" {
		t.Errorf("truncate = %q, want it unchanged", got)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// A guard on the seam the whole domain reads through: a walk of the live /proc
// must not happen just because somebody asked what this build can check.
func TestTheCheckIsConstructedWithoutTouchingTheHost(t *testing.T) {
	src := source(stage())
	src.FS = refusingFS{}
	_ = Checks(src)[0].Spec()
}

// refusingFS fails every read, so a test that expected no I/O finds out.
type refusingFS struct{}

func (refusingFS) Open(string) (fs.File, error) { return nil, fs.ErrPermission }
