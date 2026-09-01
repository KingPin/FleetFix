package network

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// TargetParam is the traceroute check's one argument.
const TargetParam = "target"

type traceroute struct {
	p      *netprobe.Prober
	probes corenet.Probes
}

func (c traceroute) Spec() check.Spec {
	return check.Spec{
		ID:     TracerouteID,
		Title:  "Path to a target",
		Domain: "network",
		// No NeedsBins: there are two acceptable binaries and either will do, so
		// gating on one name would report unavailable on a host that has the
		// other. SelectTraceTool answers it, and says so with the apt line.
		Params: []check.ParamSpec{{
			Name:        TargetParam,
			Description: "host or address to trace a path to",
			// Defaulted rather than required, so `--check network.traceroute`
			// works with no arguments and traces the same target the ladder's
			// internet rung pings -- which is the trace an operator wants after
			// that rung fails.
			Default: c.probes.Ladder.InternetTarget,
		}},
		Budget: c.budget(),
		// Out of the default run on cost alone: a dark path costs the full wall
		// clock below, and a fleet-wide `fleetfix check` is not the place to
		// spend half a minute per host on a diagnostic that only means something
		// once something else has already failed.
		InDefault: false,
	}
}

// budget is the slower of the two tools' wall clocks.
//
// Which tool will run is not known until the check does, and budgeting for
// traceroute on a host that only has tracepath would cut the trace off at a
// third of the time tracepath needs.
func (c traceroute) budget() time.Duration {
	seconds := max(
		netprobe.TraceTimeout(corenet.TracerouteTool, c.probes.Traceroute),
		netprobe.TraceTimeout(corenet.TracepathTool, c.probes.Traceroute),
	)
	return time.Duration(seconds*float64(time.Second)) + slack
}

func (c traceroute) Run(ctx context.Context, in check.Input) check.Result {
	target := in.Param(TargetParam)
	if target == "" {
		// Only reachable when an operator passes --param target= with nothing
		// after it, since the spec carries a default. Skipped rather than
		// errored: an empty argument is a question that was never asked.
		return check.Result{
			Status:  check.StatusSkipped,
			Summary: "no target to trace",
		}
	}

	tool, ok := c.p.SelectTraceTool()
	if !ok {
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: "no trace tool installed",
			Error:   netprobe.MissingTraceTools,
		}
	}

	// Said before the trace starts, because this check can sit silent for half a
	// minute and a TUI row that does not say why reads as a hang.
	in.Progress.Emit(check.Event{
		Text: fmt.Sprintf("%s to %s, up to %d hops (up to %gs)",
			tool, target, c.probes.Traceroute.MaxHops,
			netprobe.TraceTimeout(tool, c.probes.Traceroute)),
		Status: check.StatusOK,
	})

	result := c.p.Trace(ctx, target, c.probes.Traceroute)
	res := check.Result{Data: result}

	// Emitted rather than appended to res.Steps: the runner keeps a check's own
	// Steps if it built any, so building the slice here would drop the opening
	// line above and leave steps[] unable to say which tool ran or against what
	// budget -- while a UI that renders both the stream and steps[] showed every
	// hop twice.
	for _, hop := range result.Hops {
		in.Progress.Emit(check.Event{Text: hopLine(hop), Status: hopStatus(hop)})
	}
	if result.Error != nil {
		// Kept alongside the hops rather than instead of them: a partial trace
		// still carries the diagnostic, and the error says why it is partial.
		in.Progress.Emit(check.Event{Text: *result.Error, Status: check.StatusWarn})
	}

	res.Metrics = []check.Metric{
		gauge(TraceHopsMetric, float64(len(result.Hops)), "count",
			map[string]string{"target": target}, "hops the trace collected"),
	}

	// v1's _trace_verdict, in order.
	switch stalled, dark := result.StalledAt(); {
	case len(result.Hops) == 0:
		// An unknown host, or a wall clock hit before hop 1.
		res.Status = check.StatusCrit
		res.Summary = fmt.Sprintf("trace %s: %s", target, orText(result.Error, "no hops came back"))
	case result.Reached:
		last := result.Hops[len(result.Hops)-1]
		rtt := "no rtt"
		if len(last.RTTsMS) > 0 {
			rtt = fmt.Sprintf("%.1fms", last.RTTsMS[0])
		}
		res.Status = check.StatusOK
		res.Summary = fmt.Sprintf("trace %s — reached in %d hops (%s)", target, last.Number, rtt)
	case dark:
		// Warn, not fail: transit routers that rate-limit or drop ICMP
		// TTL-exceeded are extremely common, and a dark path past hop 6 says
		// nothing about whether the destination itself is reachable -- that is
		// what ping answers.
		res.Status = check.StatusWarn
		res.Summary = fmt.Sprintf(
			"trace %s — path goes dark after hop %d (no reply from hop %d to %d)",
			target, stalled-1, stalled, result.MaxHops,
		)
	default:
		res.Status = check.StatusCrit
		res.Summary = fmt.Sprintf("trace %s — nothing answered in %d hops", target, result.MaxHops)
	}
	return res
}

// hopStatus marks a hop that answered against one that did not.
//
// A silent hop is a warning and never a failure: one dark hop in the middle of a
// path that reaches its destination is a router configured not to reply, not a
// fault. Whether the darkness mattered is the summary's verdict, not the hop's.
func hopStatus(hop corenet.TraceHop) check.Status {
	if hop.Responded() {
		return check.StatusOK
	}
	return check.StatusWarn
}

// hopLine is v1's _hop_line, which is the trace rendered the way traceroute
// itself renders it: number, who answered, how long they took, and what the
// tool annotated the hop with.
func hopLine(hop corenet.TraceHop) string {
	parts := []string{fmt.Sprintf("%3d", hop.Number)}
	if hop.Responded() {
		rtts := make([]string, 0, len(hop.RTTsMS))
		for _, rtt := range hop.RTTsMS {
			rtts = append(rtts, fmt.Sprintf("%.1fms", rtt))
		}
		if len(rtts) == 0 {
			// A hop that answered with no timing: the responder is the finding,
			// and an empty column here would read as a missing one.
			rtts = []string{"no rtt"}
		}
		parts = append(parts, strings.Join(hop.Hosts, " "), strings.Join(rtts, " "))
	}
	if hop.Timeouts > 0 {
		parts = append(parts, strings.TrimSpace(strings.Repeat("* ", hop.Timeouts)))
	}
	if len(hop.Flags) > 0 {
		parts = append(parts, strings.Join(hop.Flags, " "))
	}
	return strings.Join(parts, "  ")
}
