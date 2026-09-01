package system

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// The /proc fixtures are the M0 corpus, the same bytes internal/core/system's
// table tests and the Python oracle read. Nothing here restates what the parsers
// produce -- that is already pinned against Python -- so these tests are about the
// layer the port adds: which status, which trips, which metrics, and what the
// summary says.
//
// The thermal zones are built here rather than taken from the corpus, because
// testdata/thermal/zones.json is rooted at a directory the reader is pointed
// straight at, and this domain points it at class/thermal under a Sys root. The
// parser's treatment of a malformed zone is core's business and pinned there; what
// these need is a known set of temperatures to grade.

// procWith stages a /proc holding the named corpus files, keyed by the names the
// readers look for.
func procWith(t *testing.T, files map[string]string) fstest.MapFS {
	t.Helper()
	out := fstest.MapFS{}
	for name, rel := range files {
		out[name] = &fstest.MapFile{Data: []byte(fixture.Text(t, rel))}
	}
	return out
}

// fullProc is a host that answers all three /proc reads: load 0.12/0.34/0.56,
// 16 GB of memory half used, up for three and a half hours.
func fullProc(t *testing.T) fstest.MapFS {
	t.Helper()
	return procWith(t, map[string]string{
		coresystem.ProcLoadavg: "proc/loadavg.txt",
		coresystem.ProcMeminfo: "proc/meminfo/full.txt",
		coresystem.ProcUptime:  "proc/uptime.txt",
	})
}

// A zone is one sysfs thermal_zone as a test stages it.
type zone struct {
	name  string
	kind  string
	milli int
}

// sysWithZones stages class/thermal. A zone with no kind gets no type file,
// which is the shape a driver that registered without one leaves behind.
func sysWithZones(zones ...zone) fstest.MapFS {
	out := fstest.MapFS{}
	for _, z := range zones {
		dir := path.Join(coresystem.SysThermal, z.name)
		out[path.Join(dir, "temp")] = &fstest.MapFile{Data: []byte(strconv.Itoa(z.milli) + "\n")}
		if z.kind != "" {
			out[path.Join(dir, "type")] = &fstest.MapFile{Data: []byte(z.kind + "\n")}
		}
	}
	return out
}

// staged builds a Source over the given filesystems, with everything else inert:
// four CPUs, a runner that answers nothing, and no notifier file. A test that
// wants one of those says so.
func staged(proc, sys fs.FS) Source {
	return Source{
		Host:         hostfs.Host{Proc: proc, Sys: sys},
		Run:          cmdrun.NewFake(),
		CPUs:         func() int { return 4 },
		NotifierPath: DefaultNotifierPath,
		ReadFile:     func(string) (string, error) { return "", errors.New("no such file") },
	}
}

func byID(t *testing.T, id check.ID, src Source) check.Check {
	t.Helper()
	for _, c := range Checks(src) {
		if c.Spec().ID == id {
			return c
		}
	}
	t.Fatalf("no check with id %s", id)
	return nil
}

// run drives one check and returns the normalised Result alongside what it
// emitted. Normalize because the runner applies it to everything on its way out,
// and asserting on a Result the report would never carry tests a shape nothing
// ships.
func run(t *testing.T, id check.ID, src Source) (check.Result, []check.Event) {
	t.Helper()
	return runGraded(t, id, src, threshold.Defaults())
}

func runGraded(t *testing.T, id check.ID, src Source, set threshold.Set) (check.Result, []check.Event) {
	t.Helper()
	var steps []check.Event
	res := byID(t, id, src).Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.EmitterFunc(func(e check.Event) { steps = append(steps, e) }),
		Thresholds: set,
	})
	return res.Normalize(), steps
}

func metricNamed(t *testing.T, res check.Result, name string) check.Metric {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no %s metric among %v", name, res.Metrics)
	return check.Metric{}
}

// The domain's shape, asserted once.
func TestTheDomainShipsFiveChecks(t *testing.T) {
	checks := Checks(staged(fstest.MapFS{}, fstest.MapFS{}))

	if len(checks) != 5 {
		t.Fatalf("the domain has %d checks, want 5", len(checks))
	}
	seen := map[check.ID]bool{}
	for _, c := range checks {
		spec := c.Spec()
		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", spec.ID, err)
		}
		if spec.Domain != "system" {
			t.Errorf("%s is in domain %q", spec.ID, spec.Domain)
		}
		if !spec.InDefault {
			t.Errorf("%s is not in the default run", spec.ID)
		}
		if seen[spec.ID] {
			t.Errorf("%s is registered twice", spec.ID)
		}
		seen[spec.ID] = true
	}
	for _, id := range []check.ID{LoadID, MemoryID, ThermalID, UpdatesID, UptimeID} {
		if !seen[id] {
			t.Errorf("%s is missing from the domain", id)
		}
	}
}

// A policy with no rule for what a check measures is an error, not a green host.
// Four of the five grade something, and each has to refuse rather than report ok
// against a bound nobody set.
func TestAMissingRuleIsAnErrorRatherThanASilentPass(t *testing.T) {
	for _, tc := range []struct {
		id   check.ID
		rule string
	}{
		{LoadID, threshold.LoadPerCPU},
		{MemoryID, threshold.MemUsedPct},
		{ThermalID, threshold.ThermalTempC},
		{UpdatesID, threshold.UpdatesSecure},
	} {
		res, _ := runGraded(t, tc.id, staged(fullProc(t), fstest.MapFS{}), threshold.Set{})
		if res.Status != check.StatusError {
			t.Errorf("%s with no rules = %s, want error", tc.id, res.Status)
		}
		if !contains(res.Error, tc.rule) {
			t.Errorf("%s error = %q, want the missing rule %s named", tc.id, res.Error, tc.rule)
		}
	}
}

// And the refusal happens before the host is touched, since the answer cannot
// depend on it: an unreadable /proc must not turn "nobody configured this" into
// "this file would not open".
func TestTheMissingRuleIsReportedWithoutReadingTheHost(t *testing.T) {
	res, _ := runGraded(t, LoadID, staged(fstest.MapFS{}, fstest.MapFS{}), threshold.Set{})

	if !contains(res.Error, threshold.LoadPerCPU) {
		t.Errorf("error = %q, want the missing rule rather than a read failure", res.Error)
	}
}

// New is the live host, and every seam in it has to be set: a nil one is a panic
// on the first host that reaches it, which for the notifier reader is every
// Ubuntu box in the fleet.
func TestNewWiresEverySeam(t *testing.T) {
	src := New()

	switch {
	case src.Host.Proc == nil || src.Host.Sys == nil:
		t.Error("New left a hostfs root nil")
	case src.Run == nil:
		t.Error("New left the runner nil")
	case src.CPUs == nil:
		t.Error("New left the CPU count nil")
	case src.ReadFile == nil:
		t.Error("New left the notifier reader nil")
	case src.NotifierPath != DefaultNotifierPath:
		t.Errorf("New reads the notifier from %q", src.NotifierPath)
	}
	if got := src.CPUs(); got < 1 {
		t.Errorf("the live CPU count is %d", got)
	}
}

// The divisor is floored at one. A zero would divide the load by nothing and
// report every host at +Inf per CPU, which grades crit -- the loudest possible
// way to be wrong.
func TestTheCPUDivisorIsFlooredAtOne(t *testing.T) {
	for _, tc := range []struct {
		name string
		cpus func() int
		want int
	}{
		{"zero", func() int { return 0 }, 1},
		{"negative", func() int { return -8 }, 1},
		{"unset", nil, 1},
		{"real", func() int { return 16 }, 16},
	} {
		if got := (Source{CPUs: tc.cpus}).cpuCount(); got != tc.want {
			t.Errorf("%s: cpuCount() = %d, want %d", tc.name, got, tc.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "0 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1024 * 1024, "1.0 MB"},
		{16384000 * 1024, "15.6 GB"},
		// Past the last unit the loop stops dividing rather than growing one.
		{1024 * 1024 * 1024 * 1024 * 1024 * 1024, "1024.0 PB"},
	} {
		if got := humanBytes(tc.n); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestPlurals(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{plural(1, "CPU"), "1 CPU"},
		{plural(0, "CPU"), "0 CPUs"},
		{plural(4, "CPU"), "4 CPUs"},
		{plural(1, "package"), "1 package"},
		{plural(0, "package"), "0 packages"},
		{plural(14, "package"), "14 packages"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestFirstLineFallsBackWhenThereIsNothingToQuote(t *testing.T) {
	if got := firstLine("  \n\n ", "exited 2"); got != "exited 2" {
		t.Errorf("firstLine(whitespace) = %q, want the fallback", got)
	}
	if got := firstLine("\n  real complaint  \nsecond", ""); got != "real complaint" {
		t.Errorf("firstLine = %q, want the first non-blank line trimmed", got)
	}
}

// The live notifier reader, against a real file. os.ReadFile rather than an
// fs.FS because fs.FS paths are relative by contract and this one is named
// absolutely -- and a reader that cannot open an absolute path would send every
// Ubuntu host down the apt fallback.
func TestTheLiveNotifierReaderOpensAnAbsolutePath(t *testing.T) {
	name := path.Join(t.TempDir(), "updates-available")
	if err := os.WriteFile(name, []byte("7 packages can be updated.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := readFile(name)
	if err != nil {
		t.Fatalf("readFile(%s): %v", name, err)
	}
	if got != "7 packages can be updated.\n" {
		t.Errorf("readFile = %q", got)
	}
	if _, err := readFile(path.Join(t.TempDir(), "absent")); err == nil {
		t.Error("readFile reported no error for a file that is not there")
	}
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
