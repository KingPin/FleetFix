package docker

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coredocker "github.com/KingPin/FleetFix/v2/internal/core/docker"
)

// dfArgs is v1's, and dfBudget is v1's timeout (modules/docker/hygiene.py:124).
// `system df` walks every layer and volume on the host, so it is the slowest
// question in the domain by a wide margin on a machine with a large image cache.
var dfArgs = []string{"system", "df", "--format", "{{json .}}"}

const dfBudget = 10 * time.Second

type diskUsage struct {
	run     cmdrun.Runner
	runtime Runtime
}

func (diskUsage) Spec() check.Spec {
	return check.Spec{
		ID:        DiskID,
		Title:     "Container disk usage",
		Domain:    "docker",
		Budget:    dfBudget,
		InDefault: true,
	}
}

func (c diskUsage) Run(ctx context.Context, in check.Input) check.Result {
	rt := c.runtime(ctx)
	if !rt.Available {
		return unavailableBecause(rt)
	}

	out, err := c.run.Run(ctx, rt.Bin, dfArgs...)
	switch {
	case err != nil:
		return check.Result{
			Status:  check.StatusError,
			Summary: rt.Bin + " system df did not run",
			Error:   err.Error(),
		}
	case !out.OK():
		return check.Result{
			Status:  check.StatusError,
			Summary: rt.Bin + " system df failed",
			Error:   firstLine(out.Combined(), fmt.Sprintf("exited %d", out.ExitCode)),
		}
	}

	rows := coredocker.ParseSystemDFJSONLines(out.Stdout)
	if len(rows) == 0 {
		// The daemon answered with nothing this parser could read. Not ok: `system
		// df` on a working daemon always prints its four categories, even when every
		// one is empty, so no rows means the output was not what we can read -- a
		// dialect change, or a podman reached through a docker-named symlink.
		return check.Result{
			Status:  check.StatusError,
			Summary: rt.Bin + " system df reported no categories",
			Error:   "no line of the output parsed as a category",
		}
	}

	res := check.Result{Status: check.StatusOK, Data: rows}
	var total, reclaimable int64
	for _, row := range rows {
		total += row.SizeBytes
		reclaimable += row.ReclaimableBytes
		labels := map[string]string{"type": row.Type}
		res.Metrics = append(
			res.Metrics,
			gauge(DiskBytesMetric, float64(row.SizeBytes), "bytes", labels, "space this category holds"),
			gauge(ReclaimableMetric, float64(row.ReclaimableBytes), "bytes", labels, "space a prune would return"),
			gauge(ReclaimablePct, float64(row.ReclaimablePct), "%", labels, "share of this category a prune would return"),
		)
		in.Progress.Emit(check.Event{Text: describe(row), Status: check.StatusOK})
	}

	// ok whatever the numbers say. Nothing here is graded -- see the package doc --
	// so the summary reports the two figures an operator decides on and leaves the
	// deciding to them.
	res.Summary = fmt.Sprintf("%s across %s, %s reclaimable",
		humanBytes(total), pluralAs(len(rows), "category", "categories"), humanBytes(reclaimable))
	return res
}

// describe is one category's line, in the shape v1's table drew it: what it holds,
// how much of it is in use, and what a prune would give back.
//
// The active-of-total count is docker's own and does not mean what the words
// suggest for every category -- "Build Cache" reports 0 active on a host with a
// warm cache -- so it is reported rather than interpreted.
func describe(row coredocker.DfRow) string {
	name := strings.TrimSpace(row.Type)
	if name == "" {
		// docker names every category it prints; an unnamed one is a line we read
		// but cannot label, and calling it nothing is better than dropping it.
		name = "unnamed category"
	}
	line := fmt.Sprintf("%s: %d of %d active, %s", name, row.Active, row.TotalCount, humanBytes(row.SizeBytes))
	if row.ReclaimableBytes == 0 {
		return line + ", nothing reclaimable"
	}
	if row.ReclaimablePct == 0 {
		// docker omits the percentage on some categories, and ParseReclaimablePct
		// cannot tell that from a real 0% -- so the number is left out rather than
		// printed as a zero the operator would read as "none".
		return fmt.Sprintf("%s, %s reclaimable", line, humanBytes(row.ReclaimableBytes))
	}
	return fmt.Sprintf("%s, %s reclaimable (%d%%)", line, humanBytes(row.ReclaimableBytes), row.ReclaimablePct)
}
