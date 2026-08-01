package system

import (
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

func procMem(text string) fstest.MapFS {
	return fstest.MapFS{coresystem.ProcMeminfo: &fstest.MapFile{Data: []byte(text)}}
}

func meminfo(totalKB, availableKB, swapTotalKB, swapFreeKB int64) fstest.MapFS {
	return procMem(fmt.Sprintf(
		"MemTotal: %d kB\nMemAvailable: %d kB\nSwapTotal: %d kB\nSwapFree: %d kB\n",
		totalKB, availableKB, swapTotalKB, swapFreeKB,
	))
}

func TestMemoryReadsTheCorpus(t *testing.T) {
	res, steps := run(t, MemoryID, staged(fullProc(t), fstest.MapFS{}))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	// 16384000 kB installed, 8192000 available, so half used; swap 4 GiB with
	// 3 GiB free.
	if want := "7.8 GB of 15.6 GB used (50%), swap 1.0 GB of 4.0 GB (25%)"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v; healthy memory has nothing to narrate", steps)
	}
}

// The kernel writes "kB" and means KiB. A metric in the kernel's units would
// disagree with every other byte series in the report by 2.4%.
func TestMemoryMetricsAreBytesNotKilobytes(t *testing.T) {
	res, _ := run(t, MemoryID, staged(fullProc(t), fstest.MapFS{}))

	for _, tc := range []struct {
		name string
		want float64
		unit string
	}{
		{MemUsedPctMetric, 50, "%"},
		{MemTotalMetric, 16384000 * 1024, "bytes"},
		{MemAvailableMetric, 8192000 * 1024, "bytes"},
		{MemUsedMetric, 8192000 * 1024, "bytes"},
		{SwapUsedPctMetric, 25, "%"},
		{SwapTotalMetric, 4194304 * 1024, "bytes"},
		{SwapUsedMetric, 1048576 * 1024, "bytes"},
	} {
		m := metricNamed(t, res, tc.name)
		if m.Value != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, m.Value, tc.want)
		}
		if m.Unit != tc.unit {
			t.Errorf("%s is in %q, want %q", tc.name, m.Unit, tc.unit)
		}
	}
	if len(res.Metrics) != 7 {
		t.Errorf("the check reported %d metrics, want 7", len(res.Metrics))
	}
	if MemUsedPctMetric != threshold.MemUsedPct {
		t.Errorf("the metric is %q and the rule is %q", MemUsedPctMetric, threshold.MemUsedPct)
	}
}

// v1's bounds: warn at 80% used, crit at 95%.
func TestMemoryGradesTheUsedPercentage(t *testing.T) {
	for _, tc := range []struct {
		name             string
		total, available int64
		status           check.Status
	}{
		{"half used", 1000, 500, check.StatusOK},
		{"at the warn bound", 1000, 200, check.StatusWarn},
		{"at the crit bound", 1000, 50, check.StatusCrit},
		{"nothing available", 1000, 0, check.StatusCrit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := run(t, MemoryID, staged(meminfo(tc.total, tc.available, 0, 0), fstest.MapFS{}))
			if res.Status != tc.status {
				t.Errorf("status = %s, want %s (summary %q)", res.Status, tc.status, res.Summary)
			}
			if tc.status == check.StatusOK {
				return
			}
			if len(res.Trips) != 1 || res.Trips[0].Subject != "memory" {
				t.Errorf("trips = %+v", res.Trips)
			}
		})
	}
}

// Swap is reported and not graded, so a host that swapped out an idle daemon a
// week ago is not amber for it.
func TestFullSwapDoesNotDowngradeAHostWithFreeMemory(t *testing.T) {
	res, _ := run(t, MemoryID, staged(meminfo(1000, 900, 2048, 0), fstest.MapFS{}))

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok; swap has no rule to trip", res.Status)
	}
	if got := metricNamed(t, res, SwapUsedPctMetric).Value; got != 100 {
		t.Errorf("%s = %v, want 100 -- it is still reported", SwapUsedPctMetric, got)
	}
}

// A host with no swap says so. A 0% reading there is not a reading, and an
// operator scanning the line would read it as headroom.
func TestAHostWithoutSwapSaysSoRatherThanReportingZero(t *testing.T) {
	res, _ := run(t, MemoryID, staged(procMem(fixture.Text(t, "proc/meminfo/no_swap.txt")), fstest.MapFS{}))

	if want := "500.0 KB of 1000.0 KB used (50%), no swap configured"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if got := metricNamed(t, res, SwapTotalMetric).Value; got != 0 {
		t.Errorf("%s = %v", SwapTotalMetric, got)
	}
}

// The reader has a meaningful zero -- an unparseable meminfo is all zeros, not an
// error -- and UsedPct is 0 for it, which grades ok. A host reporting no memory
// at all has a /proc that did not answer, and reporting that as healthy is the
// permanently-green failure this layer exists to prevent.
func TestAHostReportingNoMemoryIsAnErrorRatherThanZeroPercentUsed(t *testing.T) {
	for _, name := range []string{"", "nothing a parser can use\n"} {
		res, _ := run(t, MemoryID, staged(procMem(name), fstest.MapFS{}))
		if res.Status != check.StatusError {
			t.Errorf("%q graded %s, want error", name, res.Status)
		}
		if len(res.Trips) != 0 {
			t.Errorf("%q produced trips %v", name, res.Trips)
		}
		if len(res.Metrics) != 0 {
			t.Errorf("%q reported metrics %v for a host it could not read", name, res.Metrics)
		}
	}
}

func TestAnUnreadableMeminfoIsAnError(t *testing.T) {
	res, _ := run(t, MemoryID, staged(fstest.MapFS{}, fstest.MapFS{}))

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !contains(res.Summary, coresystem.ProcMeminfo) {
		t.Errorf("summary = %q, want the file named", res.Summary)
	}
}

// MemAvailable is what the kernel says is usable without swapping; MemFree is
// what is untouched. v1 falls back to MemFree when the first is absent, which
// matters on the pre-3.14 kernels that never wrote it.
func TestMemoryFallsBackToMemFree(t *testing.T) {
	res, _ := run(t, MemoryID, staged(procMem(fixture.Text(t, "proc/meminfo/no_memavailable.txt")), fstest.MapFS{}))

	if got := metricNamed(t, res, MemAvailableMetric).Value; got != 250*1024 {
		t.Errorf("%s = %v, want MemFree", MemAvailableMetric, got)
	}
}
