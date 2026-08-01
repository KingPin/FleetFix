package system

import (
	"context"
	"fmt"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

type memory struct{ src Source }

func (memory) Spec() check.Spec {
	return check.Spec{
		ID:        MemoryID,
		Title:     "Memory",
		Domain:    "system",
		Budget:    readBudget,
		InDefault: true,
	}
}

func (c memory) Run(ctx context.Context, in check.Input) check.Result {
	rule, known := in.Thresholds.Get(threshold.MemUsedPct)
	if !known {
		return ungraded(threshold.MemUsedPct, "memory")
	}

	mem, err := coresystem.ReadMeminfo(c.src.Host.Proc, coresystem.ProcMeminfo)
	if err != nil {
		return unreadable("/proc/"+coresystem.ProcMeminfo, err)
	}
	if mem.TotalKB == 0 {
		// The reader has a meaningful zero -- an unparseable meminfo returns all
		// zeros rather than an error -- and UsedPct is 0 for it, which grades ok.
		// A host reporting no memory at all has not got a clean bill of health; it
		// has a /proc that did not answer, and the permanently-green reading is
		// exactly what this domain must not produce.
		return check.Result{
			Status:  check.StatusError,
			Summary: "this host reported no total memory",
			Error:   "/proc/" + coresystem.ProcMeminfo + " had no readable MemTotal",
		}
	}

	usedPct := mem.UsedPct()
	res := check.Result{
		Data: mem,
		Metrics: []check.Metric{
			plain(MemUsedPctMetric, usedPct, "%", "memory in use"),
			// Bytes, where the parser keeps the kernel's kB. The parser is
			// byte-compatible with v1's dataclass and cannot change units; a metric
			// is a new contract, and bytes are what a consumer of an alerting series
			// already expects.
			plain(MemTotalMetric, kbToBytes(mem.TotalKB), "bytes", "memory installed"),
			plain(MemAvailableMetric, kbToBytes(mem.AvailableKB), "bytes", "memory available without swapping"),
			plain(MemUsedMetric, kbToBytes(mem.UsedKB), "bytes", "memory in use"),
			plain(SwapUsedPctMetric, mem.SwapUsedPct(), "%", "swap in use"),
			plain(SwapTotalMetric, kbToBytes(mem.SwapTotalKB), "bytes", "swap configured"),
			plain(SwapUsedMetric, kbToBytes(mem.SwapUsedKB), "bytes", "swap in use"),
		},
	}
	if trip, fired := rule.Check(usedPct, "memory"); fired {
		res.Trips = append(res.Trips, trip)
	}

	res.Summary = fmt.Sprintf("%s of %s used (%.0f%%), %s",
		humanBytes(mem.UsedKB*1024), humanBytes(mem.TotalKB*1024), usedPct, describeSwap(mem))
	return res
}

// describeSwap is the summary's second clause.
//
// Reported but not graded: threshold has no swap rule, and the one v1 would have
// implied does not exist either. Swap at 100% is normal on a host that swapped
// out an idle daemon a week ago and has not needed it since, and swap at 0% on a
// host with none configured is not a reading at all -- which is why a host without
// swap says so rather than reporting a zero an operator would read as headroom.
func describeSwap(mem coresystem.MemoryInfo) string {
	if mem.SwapTotalKB == 0 {
		return "no swap configured"
	}
	return fmt.Sprintf("swap %s of %s (%.0f%%)",
		humanBytes(mem.SwapUsedKB*1024), humanBytes(mem.SwapTotalKB*1024), mem.SwapUsedPct())
}
