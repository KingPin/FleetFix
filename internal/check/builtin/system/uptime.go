package system

import (
	"context"
	"fmt"
	"math"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
)

type uptime struct{ src Source }

func (uptime) Spec() check.Spec {
	return check.Spec{
		ID:        UptimeID,
		Title:     "Uptime",
		Domain:    "system",
		Budget:    readBudget,
		InDefault: true,
	}
}

// Run reports how long this host has been up, and grades nothing.
//
// There is no threshold rule for uptime and inventing one would be wrong in both
// directions: a short uptime is a routine reboot on a host that was patched last
// night and an unplanned crash on one that was not, and this check cannot tell
// them apart. A long uptime is a stable host or one that has been running an
// unpatched kernel for two years, and it cannot tell those apart either.
//
// It is a check rather than a field on the envelope because it is the number an
// operator correlates everything else against -- a load spike four minutes into a
// boot is a host still starting up -- and because a fleet querying reports for
// "which hosts rebooted since Tuesday" wants a series, not a sentence.
func (c uptime) Run(ctx context.Context, in check.Input) check.Result {
	secs, err := coresystem.ReadUptime(c.src.Host.Proc, coresystem.ProcUptime)
	if err != nil {
		return unreadable("/proc/"+coresystem.ProcUptime, err)
	}
	if math.IsNaN(secs) || math.IsInf(secs, 0) || secs < 0 {
		// The reader accepts what Python's float() accepts, so "nan" and "-1"
		// arrive here from a staged or namespaced /proc. Rendering either would
		// mean converting a non-number to an int, which is undefined in Go and a
		// ValueError in v1 -- and a negative uptime rendered through floor division
		// reads as a plausible-looking duration rather than as nonsense.
		return check.Result{
			Status:  check.StatusError,
			Summary: "this host's uptime is not a duration",
			Error:   fmt.Sprintf("/proc/%s reported %v seconds", coresystem.ProcUptime, secs),
		}
	}

	return check.Result{
		Status:  check.StatusOK,
		Summary: "up " + formatUptime(secs),
		Data:    map[string]float64{"uptime_seconds": secs},
		Metrics: []check.Metric{plain(UptimeMetric, secs, "seconds", "seconds since boot")},
	}
}

// formatUptime is v1's format_uptime (modules/system/metrics.py:96): "3d 14h 22m",
// "14h 22m", or "22m 8s". The seconds field only appears under an hour, which is
// v1's choice and the useful one -- nobody reads the seconds on a host that has
// been up for a week, and everybody does on one that just came back.
func formatUptime(seconds float64) string {
	total := int64(seconds)
	days, total := total/86400, total%86400
	hours, total := total/3600, total%3600
	minutes, secs := total/60, total%60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm %ds", minutes, secs)
	}
}
