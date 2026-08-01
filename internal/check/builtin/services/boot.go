package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coreservices "github.com/KingPin/FleetFix/v2/internal/core/services"
)

// analyzeBin and blameArgs are v1's. --no-pager matters: without it systemd-analyze
// pipes into a pager when it thinks it has a terminal, and a check that waits for
// one to exit is a check that burns its whole budget.
const analyzeBin = "systemd-analyze"

var blameArgs = []string{"blame", "--no-pager"}

// stepCap bounds how many slow units get a line of their own.
//
// A host with forty units over five seconds would otherwise put forty lines in a
// document whose point is the few that matter. The remainder is stated rather
// than dropped quietly, and data[] carries every entry regardless.
const stepCap = 10

type boot struct{ run cmdrun.Runner }

func (boot) Spec() check.Spec {
	return check.Spec{
		ID:        BootID,
		Title:     "Boot time",
		Domain:    "services",
		NeedsBins: []string{analyzeBin},
		Budget:    bootBudget,
		InDefault: true,
	}
}

func (c boot) Run(ctx context.Context, in check.Input) check.Result {
	// No threshold lookup, and no ungraded() guard: this check grades nothing, so
	// there is no rule whose absence could make it silently green. See the package
	// doc for why there is no boot rule to begin with.
	out, err := c.run.Run(ctx, analyzeBin, blameArgs...)
	switch {
	case errors.Is(err, cmdrun.ErrNotFound):
		return notManagedBySystemd(analyzeBin)
	case err != nil:
		return check.Result{
			Status:  check.StatusError,
			Summary: "systemd-analyze blame did not run",
			Error:   err.Error(),
		}
	case !out.OK():
		// systemd-analyze refuses while the boot is still finishing, and inside a
		// container that has no boot to analyse. Both say so on stderr, and both are
		// worth reporting in systemd's own words rather than as "no timings".
		return check.Result{
			Status:  check.StatusError,
			Summary: "systemd-analyze blame failed",
			Error:   firstLine(out.Combined(), fmt.Sprintf("exited %d", out.ExitCode)),
		}
	}

	entries := coreservices.ParseBlame(out.Stdout)
	if len(entries) == 0 {
		// unavailable rather than ok, for thermal's reason: a host that booted has
		// timings, so an empty answer means nothing measured this boot -- an
		// nspawn container, a systemd too old to have kept them. Calling that ok
		// would report a fast boot on a host nobody timed.
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: "this host reports no boot timings",
		}
	}

	// systemd already sorted it slowest-first, and ParseBlame keeps that order, so
	// the first entry is the slowest and no sort of our own is needed -- one that
	// broke ties differently would reorder equal durations between runs and cost
	// the document its byte-stability.
	slowest := entries[0]
	slow := outliers(entries)

	// systemd prints microsecond durations for socket units -- "11us
	// systemd-rfkill.socket" -- and neither v1's parser nor this port reads a "us"
	// suffix, so those lines are dropped. That is the parser's ported behaviour and
	// stays; what does not stay is a summary claiming this host has 76 units when
	// systemd printed 110. The count is what systemd reported and the gap is
	// stated, because a reader who runs the command themselves will find it.
	reported := max(countLines(out.Stdout), len(entries))
	if missed := reported - len(entries); missed > 0 {
		// ok, not warn. Every line this costs is a sub-millisecond socket, which
		// cannot be what made a boot slow -- the note is about the reading's
		// completeness, not about the host.
		in.Progress.Emit(check.Event{
			Text: fmt.Sprintf("%d of %d timings named no duration this build understands",
				missed, reported),
			Status: check.StatusOK,
		})
	}

	res := check.Result{
		Status: check.StatusOK,
		Data:   entries,
		Metrics: []check.Metric{
			// Unlabelled, deliberately. Labelling this by unit would mint a new
			// Prometheus series every time the slowest unit changed and leave the old
			// one stale; which unit it was belongs in the summary and in data[].
			gauge(SlowestMetric, float64(slowest.DurationMS), "ms", "the slowest unit's start time"),
			gauge(SlowUnitsMetric, float64(len(slow)), "count",
				fmt.Sprintf("units that took %dms or more to start", OutlierMS)),
		},
		Summary: fmt.Sprintf("%s timed, slowest is %s at %s",
			plural(reported, "unit"), slowest.Unit, duration(slowest.DurationMS)),
	}

	// No total. Summing these would not be the boot time -- systemd starts units in
	// parallel, so the sum exceeds the wall clock by however much overlapped -- and
	// a number labelled "boot" that is not how long the boot took is worse than no
	// number. `systemd-analyze time` answers that question and is a different call.
	for i, e := range slow {
		if i == stepCap {
			in.Progress.Emit(check.Event{
				Text:   fmt.Sprintf("and %d more units over %s", len(slow)-stepCap, duration(OutlierMS)),
				Status: check.StatusOK,
			})
			break
		}
		// StatusOK, not warn: these are the narration of a reading this check does
		// not grade. A warn line under an ok result would read as a fault the status
		// forgot to mention.
		in.Progress.Emit(check.Event{
			Text:   fmt.Sprintf("%s took %s", e.Unit, duration(e.DurationMS)),
			Status: check.StatusOK,
		})
	}
	return res
}

// countLines is how many timings systemd printed.
//
// Its own count rather than the parser's, so the two can be compared. Blank lines
// do not count; a line the parser rejected does, because on a real host every
// line systemd-analyze prints is a unit.
func countLines(s string) int {
	n := 0
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}

// outliers are the units worth a line, in the order systemd reported them.
func outliers(entries []coreservices.BlameEntry) []coreservices.BlameEntry {
	out := make([]coreservices.BlameEntry, 0, len(entries))
	for _, e := range entries {
		if e.DurationMS >= OutlierMS {
			out = append(out, e)
		}
	}
	return out
}

// duration renders milliseconds the way systemd-analyze printed them, so a line
// in this report and the line an operator gets from running the command by hand
// say the same thing.
func duration(ms int64) string {
	switch {
	case ms >= 60_000:
		return fmt.Sprintf("%dmin %.3fs", ms/60_000, float64(ms%60_000)/1000)
	case ms >= 1000:
		return fmt.Sprintf("%.3fs", float64(ms)/1000)
	default:
		return fmt.Sprintf("%dms", ms)
	}
}
