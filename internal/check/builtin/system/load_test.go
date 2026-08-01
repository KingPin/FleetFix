package system

import (
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// procLoad stages a /proc/loadavg with the given one-minute figure and the
// corpus's shape around it.
func procLoad(text string) fstest.MapFS {
	return fstest.MapFS{coresystem.ProcLoadavg: &fstest.MapFile{Data: []byte(text)}}
}

func TestLoadReadsTheCorpusAndDividesByTheCPUCount(t *testing.T) {
	res, steps := run(t, LoadID, staged(fullProc(t), fstest.MapFS{}))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	// 0.12 over four CPUs. The three raw averages are in the line as well as the
	// derived one, because the derived one cannot say whether the host is
	// recovering or getting worse.
	if want := "load 0.12 0.34 0.56 across 4 CPUs, 0.03 per CPU"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v; a healthy load has nothing to narrate", steps)
	}
	if got := metricNamed(t, res, LoadPerCPUMetric).Value; got != 0.03 {
		t.Errorf("%s = %v, want 0.03", LoadPerCPUMetric, got)
	}
	if got := metricNamed(t, res, CPUCountMetric).Value; got != 4 {
		t.Errorf("%s = %v, want 4", CPUCountMetric, got)
	}
}

// The graded series is named for the rule that grades it, so a dashboard drawing
// the number and a dashboard drawing the bound are the same panel.
func TestTheGradedSeriesIsNamedForItsRule(t *testing.T) {
	if LoadPerCPUMetric != threshold.LoadPerCPU {
		t.Errorf("the metric is %q and the rule is %q", LoadPerCPUMetric, threshold.LoadPerCPU)
	}
}

func TestLoadReportsEveryAverageAndTheCount(t *testing.T) {
	res, _ := run(t, LoadID, staged(fullProc(t), fstest.MapFS{}))

	for _, tc := range []struct {
		name string
		want float64
		unit string
	}{
		{LoadPerCPUMetric, 0.03, "ratio"},
		{Load1Metric, 0.12, "count"},
		{Load5Metric, 0.34, "count"},
		{Load15Metric, 0.56, "count"},
		{CPUCountMetric, 4, "count"},
	} {
		m := metricNamed(t, res, tc.name)
		if m.Value != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, m.Value, tc.want)
		}
		if m.Unit != tc.unit {
			t.Errorf("%s is in %q, want %q", tc.name, m.Unit, tc.unit)
		}
		if m.Kind != check.Gauge {
			t.Errorf("%s is a %s", tc.name, m.Kind)
		}
		// One number about the whole host has no dimension to be labelled by, and
		// an invented one would split the series in a dashboard for no reason.
		if len(m.Labels) != 0 {
			t.Errorf("%s is labelled %v", tc.name, m.Labels)
		}
	}
	if len(res.Metrics) != 5 {
		t.Errorf("the check reported %d metrics, want 5", len(res.Metrics))
	}
}

// The bounds are v1's: warn at 1.0 per CPU, crit at 2.0.
func TestLoadTripsOnTheDerivedRatioNotTheRawAverage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		one    string
		cpus   int
		status check.Status
	}{
		// 3.9 across four CPUs is under 1.0 each: a raw average that reads alarming
		// on a laptop and is idle on a build host, which is the whole reason the
		// rule divides.
		{"busy but wide", "3.90 3.90 3.90 2/345 6789", 4, check.StatusOK},
		{"at the warn bound", "4.00 0.00 0.00 2/345 6789", 4, check.StatusWarn},
		{"at the crit bound", "8.00 0.00 0.00 2/345 6789", 4, check.StatusCrit},
		{"one core, one runnable", "1.00 0.00 0.00 2/345 6789", 1, check.StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := staged(procLoad(tc.one), fstest.MapFS{})
			src.CPUs = func() int { return tc.cpus }

			res, _ := run(t, LoadID, src)
			if res.Status != tc.status {
				t.Errorf("status = %s, want %s (summary %q)", res.Status, tc.status, res.Summary)
			}
			if tc.status == check.StatusOK {
				return
			}
			if len(res.Trips) != 1 {
				t.Fatalf("trips = %v, want exactly one", res.Trips)
			}
			if res.Trips[0].Subject != "load" {
				t.Errorf("trip subject = %q", res.Trips[0].Subject)
			}
			if res.Trips[0].Rule != threshold.LoadPerCPU {
				t.Errorf("trip rule = %q", res.Trips[0].Rule)
			}
		})
	}
}

// /proc/loadavg is not optional on Linux, so a host that cannot produce it has a
// fault -- a namespace mounted without /proc, a seccomp filter -- and not an
// absence. unavailable would keep that out of the exit code without --strict.
func TestAnUnreadableLoadavgIsAnErrorNotAnAbsence(t *testing.T) {
	res, _ := run(t, LoadID, staged(fstest.MapFS{}, fstest.MapFS{}))

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !contains(res.Summary, coresystem.ProcLoadavg) {
		t.Errorf("summary = %q, want the file named", res.Summary)
	}
	if res.Error == "" {
		t.Error("the error field is empty; nothing says why the read failed")
	}
}

// A non-finite load would compare false against both bounds and therefore report
// ok, which is the permanently-green reading this layer exists to prevent. The
// kernel cannot write it; a staged or namespaced /proc can.
func TestANonFiniteLoadIsRefusedRatherThanGraded(t *testing.T) {
	for _, one := range []string{"nan 0.0 0.0 2/3 4", "inf 0.0 0.0 2/3 4", "-inf 0.0 0.0 2/3 4"} {
		res, _ := run(t, LoadID, staged(procLoad(one), fstest.MapFS{}))
		if res.Status != check.StatusError {
			t.Errorf("%q graded %s, want error", one, res.Status)
		}
		if len(res.Trips) != 0 {
			t.Errorf("%q produced trips %v", one, res.Trips)
		}
	}
}

// data[] carries the arithmetic, not just the inputs: a reader checking why a
// host tripped should not have to know the divisor to reproduce the verdict.
func TestLoadDataShowsTheDivisionItGradedOn(t *testing.T) {
	res, _ := run(t, LoadID, staged(fullProc(t), fstest.MapFS{}))

	got, ok := res.Data.(loadReading)
	if !ok {
		t.Fatalf("data is %T, want loadReading", res.Data)
	}
	if got.CPUs != 4 || got.PerCPU != 0.03 || got.One != 0.12 {
		t.Errorf("data = %+v", got)
	}
}
