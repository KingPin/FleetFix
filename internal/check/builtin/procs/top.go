package procs

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coreprocs "github.com/KingPin/FleetFix/v2/internal/core/procs"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
)

// Report is what procs.top puts in data[].
//
// Two rankings of one snapshot, plus what the snapshot cost, so a reader can tell
// a 40% row taken over a fifth of a second from the same row taken over two.
type Report struct {
	Total      int                  `json:"total"`
	Unreadable int                  `json:"unreadable"`
	SampleMS   int64                `json:"sample_ms"`
	ByRSS      []coreprocs.ProcInfo `json:"by_rss"`
	ByCPU      []coreprocs.ProcInfo `json:"by_cpu"`
}

type top struct{ src Source }

func (top) Spec() check.Spec {
	return check.Spec{
		ID:     TopID,
		Title:  "Top processes",
		Domain: "procs",
		Budget: budget,
		Params: []check.ParamSpec{{
			Name:        TopParam,
			Description: "how many processes each ranking reports",
			Default:     strconv.Itoa(DefaultTop),
		}},
		InDefault: true,
	}
}

// Run ranks this host's processes by resident memory and by CPU, and grades
// nothing.
//
// The parameter is optional and has a default, so this stays in the default set:
// unlike the storage domain's checks, there is nothing here an operator has to
// supply before the question means anything. "Which processes are the big ones"
// has an answer on every host.
func (c top) Run(ctx context.Context, in check.Input) check.Result {
	n, res, ok := topN(in)
	if !ok {
		return res
	}

	snap, err := c.src.take(ctx)
	if err != nil {
		return failed(c.src.dir(), err)
	}
	if len(snap.ByRSS) == 0 {
		// Not an empty answer: /proc always holds at least the process doing the
		// reading, so nothing here means this is not a process table. An operator
		// who mounted a namespace's /proc somewhere unexpected gets told that
		// rather than a report of zero processes on a running host.
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: "nothing under " + c.src.dir() + " looks like a process",
		}
	}

	byRSS := snap.ByRSS[:min(n, len(snap.ByRSS))]
	byCPU := snap.ByCPU[:min(n, len(snap.ByCPU))]

	incomplete := ""
	if snap.Unreadable > 0 {
		incomplete = fmt.Sprintf("; %s could not be read", processes(snap.Unreadable))
		in.Progress.Emit(check.Event{
			Text: fmt.Sprintf("%s under %s could not be read, so these rankings are partial",
				processes(snap.Unreadable), c.src.dir()),
			Status: check.StatusWarn,
		})
	}

	narrate(in, "resident", byRSS, func(p coreprocs.ProcInfo) string { return humanBytes(p.RSSBytes) })
	narrate(in, "cpu", byCPU, func(p coreprocs.ProcInfo) string { return fmt.Sprintf("%.1f%%", p.CPUPct) })

	largest, busiest := snap.ByRSS[0], snap.ByCPU[0]
	return check.Result{
		Status: check.StatusOK,
		Summary: fmt.Sprintf("%s; largest is %s at %s, busiest is %s at %.1f%%%s",
			processes(len(snap.ByRSS)),
			largest.Comm, humanBytes(largest.RSSBytes),
			busiest.Comm, busiest.CPUPct,
			incomplete),
		Data: Report{
			Total:      len(snap.ByRSS),
			Unreadable: snap.Unreadable,
			SampleMS:   snap.Elapsed.Milliseconds(),
			ByRSS:      byRSS,
			ByCPU:      byCPU,
		},
		Metrics: []check.Metric{
			gauge(CountMetric, float64(len(snap.ByRSS)), "count", "processes this run could read"),
			gauge(UnreadableMetric, float64(snap.Unreadable), "count", "processes this run could not read"),
			gauge(TopRSSMetric, float64(largest.RSSBytes), "bytes", "resident memory held by the largest process"),
			gauge(TopCPUMetric, busiest.CPUPct, "%", "CPU used by the busiest process, per core"),
		},
	}
}

// topN reads the ranking depth. A value that is not a positive whole number is an
// error rather than a quiet fall back to the default, for the reason the storage
// domain's window is: the operator typed it, and a run that ignored it would
// answer a question nobody asked.
func topN(in check.Input) (int, check.Result, bool) {
	raw := in.Param(TopParam)
	if raw == "" {
		return DefaultTop, check.Result{}, true
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 {
		return 0, check.Result{
			Status:  check.StatusError,
			Summary: "--param " + TopParam + " must be a positive whole number",
			Error:   fmt.Sprintf("%s=%q", TopParam, raw),
		}, false
	}
	return n, check.Result{}, true
}

// narrate emits one line per ranked process, capped.
//
// The measurement leads the line so the two rankings read as two ordered lists
// rather than as one shuffled table, and so a reader scanning steps[] sees the
// numbers in a column. The cmdline is not here: it is in data[], it is often
// longer than the rest of the line put together, and comm is what an operator
// recognises a process by.
func narrate(in check.Input, kind string, rows []coreprocs.ProcInfo, value func(coreprocs.ProcInfo) string) {
	shown := min(len(rows), stepCap)
	for _, p := range rows[:shown] {
		in.Progress.Emit(check.Event{
			Text:   fmt.Sprintf("%s %s: %s (pid %d, %s)", value(p), kind, p.Comm, p.PID, owned(p.User)),
			Status: check.StatusOK,
		})
	}
	if rest := len(rows) - shown; rest > 0 {
		in.Progress.Emit(check.Event{
			Text:   fmt.Sprintf("and %s further down the %s ranking", processes(rest), kind),
			Status: check.StatusOK,
		})
	}
}

// owned names a process's user, or says that nobody could.
func owned(user *string) string {
	if user == nil {
		return "no passwd entry for its uid"
	}
	return "run by " + *user
}

// failed turns a walk that did not finish into a result.
//
// A /proc that is not there is unavailable rather than an error: it is a fact
// about the host -- a chroot, a namespace, a container built without one -- and
// the check has nothing wrong with it. A cancelled run is an error, because the
// budget ran out and the answer is missing rather than absent.
func failed(dir string, err error) check.Result {
	switch {
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return check.Result{
			Status:  check.StatusError,
			Summary: "ranking the processes under " + dir + " did not finish in time",
			Error:   err.Error(),
		}
	case hostfs.IsAbsent(err):
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: "this host has no " + dir + " to read",
			Error:   err.Error(),
		}
	case hostfs.IsDenied(err):
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: dir + " is not listable by this user",
			Error:   err.Error(),
		}
	default:
		return check.Result{
			Status:  check.StatusError,
			Summary: dir + " could not be listed",
			Error:   err.Error(),
		}
	}
}
