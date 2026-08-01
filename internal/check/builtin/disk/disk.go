// Package disk registers the disk domain's checks.
//
// Thin by design. The parsing is internal/core/disk, ported byte-for-byte and
// differentially tested against the Python; the bounds are internal/threshold,
// which holds v1's nine scattered severities in one place. What is left here is
// the part v1 never had at all: deciding what a parsed row means, in a form the
// TUI, `check --json` and the agent can each use without reimplementing it.
//
// v1's run_df (modules/disk/usage.py:99) returned an empty list for every failure
// -- df missing, df erroring, df timing out -- which reads downstream as "this
// host has no filesystems", indistinguishable from a container that really has
// none, and green either way. The statuses here separate those cases because the
// report has a status for each and a fleet has a different response to each.
package disk

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coredisk "github.com/KingPin/FleetFix/v2/internal/core/disk"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// The disk domain's check ids. Public API: they appear in checks[], in --check
// selectors, and in whatever Ansible an operator writes against them.
const (
	UsageID  check.ID = "disk.usage"
	InodesID check.ID = "disk.inodes"
)

// Metric names, likewise public: these are what a dashboard and --prom pin on.
//
// Labelled by mount rather than by device. A bind mount and its origin share a
// device, so keying on the device would silently collapse two rows into one --
// and the mount is the thing an operator acts on anyway.
const (
	UsedPctMetric    = "disk.used_pct"
	AvailBytesMetric = "disk.avail_bytes"
	InodePctMetric   = "disk.inode_used_pct"
	InodeFreeMetric  = "disk.inode_free"
)

// budget is v1's df timeout, read off the `timeout_s: int = 5` default that both
// run_df and run_df_inodes carry, rather than picked here.
const budget = 5 * time.Second

// Checks returns the disk domain's checks, wired to run their subprocesses
// through the given seam.
//
// A constructor rather than package-level registration by init(): what a build
// runs must not depend on which packages happened to be linked, and a test that
// wants these two checks and nothing else has to be able to say so.
func Checks(run cmdrun.Runner) []check.Check {
	return []check.Check{usage{run: run}, inodes{run: run}}
}

type usage struct{ run cmdrun.Runner }

func (usage) Spec() check.Spec {
	return check.Spec{
		ID:        UsageID,
		Title:     "Filesystem capacity",
		Domain:    "disk",
		NeedsBins: []string{"df"},
		Budget:    budget,
		InDefault: true,
	}
}

func (c usage) Run(ctx context.Context, in check.Input) check.Result {
	rule, known := in.Thresholds.Get(threshold.DiskUsedPct)
	if !known {
		return ungraded(threshold.DiskUsedPct, "filesystems")
	}
	out, bad, ok := readDF(ctx, c.run, "-k")
	if !ok {
		return bad
	}

	rows := coredisk.ParseDF(out.text)
	if len(rows) == 0 {
		return nothingToGrade(out, "filesystems")
	}
	out.complain(in.Progress)

	res := check.Result{Data: rows}
	for _, row := range rows {
		if trip, fired := rule.Check(float64(row.UsedPct), row.Mount); fired {
			res.Trips = append(res.Trips, trip)
		}
		res.Metrics = append(
			res.Metrics,
			gauge(UsedPctMetric, float64(row.UsedPct), "%", row.Mount, "filesystem capacity used"),
			// The parsers keep df's 1024-byte blocks because the Python dataclass
			// did and the wire output is byte-compatible with it. A metric is a new
			// contract with no such constraint, so it carries bytes -- the unit
			// every consumer of an alerting series already expects.
			gauge(AvailBytesMetric, float64(row.AvailKB)*1024, "bytes", row.Mount, "filesystem space available"),
		)
	}

	top, _ := coredisk.Fullest(rows)
	res.Summary = fmt.Sprintf("%s; fullest is %s at %d%%", plural(len(rows), "filesystem"), top.Mount, top.UsedPct)
	return res
}

type inodes struct{ run cmdrun.Runner }

func (inodes) Spec() check.Spec {
	return check.Spec{
		ID:    InodesID,
		Title: "Filesystem inodes",
		// Its own check rather than more columns on disk.usage, matching the split
		// v1 made between two modules: a host can have 90% of its space free and
		// still fail to create a file, and an operator wanting one signal and not
		// the other has to be able to select it.
		Domain:    "disk",
		NeedsBins: []string{"df"},
		Budget:    budget,
		InDefault: true,
	}
}

func (c inodes) Run(ctx context.Context, in check.Input) check.Result {
	rule, known := in.Thresholds.Get(threshold.DiskInodePct)
	if !known {
		return ungraded(threshold.DiskInodePct, "filesystems")
	}
	out, bad, ok := readDF(ctx, c.run, "-i")
	if !ok {
		return bad
	}

	rows := coredisk.ParseDFInodes(out.text)
	if len(rows) == 0 {
		return nothingToGrade(out, "filesystems with inode accounting")
	}
	out.complain(in.Progress)

	res := check.Result{Data: rows}
	worst := rows[0]
	for _, row := range rows {
		if trip, fired := rule.Check(float64(row.UsedPct), row.Mount); fired {
			res.Trips = append(res.Trips, trip)
		}
		res.Metrics = append(
			res.Metrics,
			gauge(InodePctMetric, float64(row.UsedPct), "%", row.Mount, "filesystem inodes used"),
			gauge(InodeFreeMetric, float64(row.Free), "count", row.Mount, "filesystem inodes available"),
		)
		if row.UsedPct > worst.UsedPct {
			worst = row
		}
	}

	res.Summary = fmt.Sprintf("%s; fullest is %s at %d%% of inodes",
		plural(len(rows), "filesystem"), worst.Mount, worst.UsedPct)
	return res
}

// dfOutput is what df said, and what it complained about while saying it.
type dfOutput struct {
	text string

	// complaint is df's first stderr line when it exited non-zero. df exits 1
	// having printed every mount it could read when a single one is unreadable --
	// a stale NFS handle is the routine cause -- so a non-zero exit is not on its
	// own evidence that the output is worthless.
	complaint string
}

// complain records df's grumble as a step, when there was one.
//
// A step rather than a status: the check did grade every mount df managed to
// report, and downgrading the host because one mount out of forty is stale would
// put a permanent warn on hosts whose disks are fine. The operator still sees it,
// in steps[] and in the TUI, which is the point.
func (o dfOutput) complain(to check.Emitter) {
	if o.complaint == "" {
		return
	}
	to.Emit(check.Event{Text: "df: " + o.complaint, Status: check.StatusWarn})
}

// readDF runs df and returns what it printed. The bool is false when there is
// nothing to parse, and the Result then says why in the report's own terms.
func readDF(ctx context.Context, run cmdrun.Runner, flag string) (dfOutput, check.Result, bool) {
	// -P is POSIX mode: six columns, one line per row however long the device
	// name is. The parsers depend on it, and the fixture corpus was captured with
	// it, so the argv is as load-bearing as the parsing.
	res, err := run.Run(ctx, "df", "-P", flag)
	switch {
	case errors.Is(err, cmdrun.ErrNotFound):
		// The runner's NeedsBins gate answers this first on any normal path. Kept
		// because a Runner with no Looker gates nothing, and because df can be
		// removed between the lookup and the call.
		return dfOutput{}, check.Result{
			Status:  check.StatusUnavailable,
			Summary: "df is not installed on this host",
		}, false
	case err != nil:
		return dfOutput{}, check.Result{
			Status:  check.StatusError,
			Summary: "df did not run",
			Error:   err.Error(),
		}, false
	case !res.OK():
		return dfOutput{text: res.Stdout, complaint: firstLine(res.Stderr, fmt.Sprintf("exited %d", res.ExitCode))}, check.Result{}, true
	}
	return dfOutput{text: res.Stdout}, check.Result{}, true
}

// nothingToGrade is the answer when df printed no row this check could use.
//
// Which of the two statuses depends on whether df complained. Silence means the
// host genuinely has nothing of this kind -- a container whose only mounts are
// overlay and tmpfs, all of which the parsers skip -- and an absence is not a
// fault. A complaint means df was stopped from telling us, which is.
func nothingToGrade(out dfOutput, kind string) check.Result {
	if out.complaint != "" {
		return check.Result{
			Status:  check.StatusError,
			Summary: "df reported no " + kind,
			Error:   "df: " + out.complaint,
		}
	}
	return check.Result{
		Status:  check.StatusUnavailable,
		Summary: "df reported no " + kind,
	}
}

// ungraded is the answer when the grading policy has no rule for what this check
// measures.
//
// error, not ok. threshold.Merge guarantees every shipped rule survives whatever
// an operator wrote, so arriving here means a Runner was wired with a policy that
// is not the host's -- and a check that measured 99% and called it fine because
// nothing graded it is the exact permanently-green failure this layer exists to
// prevent. Reported before df runs, since the answer cannot depend on the host.
func ungraded(rule, kind string) check.Result {
	return check.Result{
		Status:  check.StatusError,
		Summary: "nothing graded these " + kind,
		Error:   fmt.Sprintf("the grading policy has no %s rule", rule),
	}
}

func gauge(name string, value float64, unit, mount, help string) check.Metric {
	return check.Metric{
		Name:   name,
		Value:  value,
		Unit:   unit,
		Labels: map[string]string{"mount": mount},
		Kind:   check.Gauge,
		Help:   help,
	}
}

// firstLine is one line of df's complaint. df prints a line per unreadable mount,
// and a summary field carrying forty of them is a field nobody reads.
func firstLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
