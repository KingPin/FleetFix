package system

import (
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

func procUptime(text string) fstest.MapFS {
	return fstest.MapFS{coresystem.ProcUptime: &fstest.MapFile{Data: []byte(text)}}
}

func TestUptimeReadsTheCorpus(t *testing.T) {
	res, steps := run(t, UptimeID, staged(fullProc(t), fstest.MapFS{}))

	// 12345.67 seconds: three hours and change.
	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	if want := "up 3h 25m"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v", steps)
	}
	m := metricNamed(t, res, UptimeMetric)
	if m.Value != 12345.67 {
		t.Errorf("%s = %v, want the raw seconds", UptimeMetric, m.Value)
	}
	if m.Unit != "seconds" || m.Kind != check.Gauge {
		t.Errorf("%s is a %s in %q", UptimeMetric, m.Kind, m.Unit)
	}
}

// Nothing grades uptime, and it stays ok whatever it reads. A short uptime is a
// planned reboot on a host patched last night and a crash on one that was not,
// and this check cannot tell them apart; a long one is a stable host or a
// two-year-old kernel, and it cannot tell those apart either.
func TestUptimeGradesNothing(t *testing.T) {
	for _, secs := range []string{"0.0 0.0", "1.0 1.0", "315360000.0 1.0"} {
		res, _ := run(t, UptimeID, staged(procUptime(secs), fstest.MapFS{}))
		if res.Status != check.StatusOK {
			t.Errorf("%q graded %s, want ok", secs, res.Status)
		}
		if len(res.Trips) != 0 {
			t.Errorf("%q produced trips %v", secs, res.Trips)
		}
	}

	// And it needs no rule to run, unlike the other four -- an empty policy must
	// not stop a host reporting when it booted.
	res, _ := runGraded(t, UptimeID, staged(fullProc(t), fstest.MapFS{}), threshold.Set{})
	if res.Status != check.StatusOK {
		t.Errorf("with no policy at all, status = %s", res.Status)
	}
}

// v1's format_uptime (modules/system/metrics.py:96). The seconds field only
// appears under an hour, which is the useful choice: nobody reads them on a host
// up for a week and everybody does on one that just came back.
func TestFormatUptimeMatchesV1(t *testing.T) {
	for _, tc := range []struct {
		secs float64
		want string
	}{
		{0, "0m 0s"},
		{8, "0m 8s"},
		{68, "1m 8s"},
		{3599.9, "59m 59s"},
		{3600, "1h 0m"},
		{12345.67, "3h 25m"},
		{86399, "23h 59m"},
		{86400, "1d 0h 0m"},
		{308520, "3d 13h 42m"},
	} {
		if got := formatUptime(tc.secs); got != tc.want {
			t.Errorf("formatUptime(%v) = %q, want %q", tc.secs, got, tc.want)
		}
	}
}

func TestAnUnreadableUptimeIsAnError(t *testing.T) {
	res, _ := run(t, UptimeID, staged(fstest.MapFS{}, fstest.MapFS{}))

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !contains(res.Summary, coresystem.ProcUptime) {
		t.Errorf("summary = %q, want the file named", res.Summary)
	}
}

// A non-finite or negative uptime is not a duration. Rendering either means
// converting a non-number to an int -- undefined in Go, a ValueError in v1 --
// and a negative one floors into a plausible-looking duration rather than
// reading as nonsense.
func TestANonsensicalUptimeIsRefusedRatherThanRendered(t *testing.T) {
	for _, secs := range []string{"nan 0.0", "inf 0.0", "-inf 0.0", "-3600.0 0.0"} {
		res, _ := run(t, UptimeID, staged(procUptime(secs), fstest.MapFS{}))
		if res.Status != check.StatusError {
			t.Errorf("%q reported %s, want error", secs, res.Status)
		}
		if len(res.Metrics) != 0 {
			t.Errorf("%q reported metrics %v", secs, res.Metrics)
		}
	}
}
