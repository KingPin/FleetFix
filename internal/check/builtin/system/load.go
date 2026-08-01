package system

import (
	"context"
	"fmt"
	"math"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

type load struct{ src Source }

func (load) Spec() check.Spec {
	return check.Spec{
		ID:        LoadID,
		Title:     "CPU load",
		Domain:    "system",
		Budget:    readBudget,
		InDefault: true,
	}
}

func (c load) Run(ctx context.Context, in check.Input) check.Result {
	rule, known := in.Thresholds.Get(threshold.LoadPerCPU)
	if !known {
		return ungraded(threshold.LoadPerCPU, "load")
	}

	avg, err := coresystem.ReadLoadavg(c.src.Host.Proc, coresystem.ProcLoadavg)
	if err != nil {
		return unreadable("/proc/"+coresystem.ProcLoadavg, err)
	}

	cpus := c.src.cpuCount()
	perCPU := avg.One / float64(cpus)
	if math.IsNaN(perCPU) || math.IsInf(perCPU, 0) {
		// /proc/loadavg is written by the kernel and cannot say "nan", but the
		// reader accepts what Python's float() accepts, so the value can arrive
		// here from a staged or namespaced /proc. Grading it would compare a
		// non-number to a bound, which is false for both and therefore reports ok.
		return check.Result{
			Status:  check.StatusError,
			Summary: "the load average is not a number",
			Error:   fmt.Sprintf("/proc/%s reported a one-minute load of %v", coresystem.ProcLoadavg, avg.One),
		}
	}

	res := check.Result{
		Data: loadReading{LoadAverage: avg, CPUs: cpus, PerCPU: perCPU},
		Metrics: []check.Metric{
			// Labelled by nothing and graded against the rule of the same name, so
			// a panel drawing this series and a panel drawing the bound are the
			// same panel.
			plain(LoadPerCPUMetric, perCPU, "ratio", "one-minute load average divided by CPU count"),
			plain(Load1Metric, avg.One, "count", "one-minute load average"),
			plain(Load5Metric, avg.Five, "count", "five-minute load average"),
			plain(Load15Metric, avg.Fifteen, "count", "fifteen-minute load average"),
			plain(CPUCountMetric, float64(cpus), "count", "CPUs this process can run on"),
		},
	}
	if trip, fired := rule.Check(perCPU, "load"); fired {
		res.Trips = append(res.Trips, trip)
	}

	// The three raw averages and the derived one, because the derived one alone
	// cannot answer the question an operator asks next: a host at 2.0 per CPU that
	// is falling has already recovered, and one that is rising has not.
	res.Summary = fmt.Sprintf("load %.2f %.2f %.2f across %s, %.2f per CPU",
		avg.One, avg.Five, avg.Fifteen, plural(int64(cpus), "CPU"), perCPU)
	return res
}

// loadReading is what the check saw, in data[]. The averages alone would leave a
// reader unable to check the arithmetic that produced the verdict.
type loadReading struct {
	coresystem.LoadAverage
	CPUs   int     `json:"cpus"`
	PerCPU float64 `json:"per_cpu"`
}
