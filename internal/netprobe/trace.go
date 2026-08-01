package netprobe

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// MissingTraceTools is what a host with neither trace binary is told, wording and
// apt lines from v1. It names the fix because "traceroute unavailable" on its own
// sends an operator to a search engine for something one command solves.
const MissingTraceTools = "neither traceroute nor tracepath is installed — " +
	"install one with: apt install traceroute (or: apt install iputils-tracepath)"

// traceBatch is traceroute's -N: how many probes it has in flight at once.
//
// Restated on the argv rather than left to the distro default, because the wall
// clock below is computed from it. A build that batched 8 would take twice as long
// as the budget allows and be killed mid-trace on every dark path.
const traceBatch = 16

// SelectTraceTool names the trace binary this host has, preferring traceroute. The
// second return is false when neither is installed.
//
// traceroute first because it is the better tool for the question: -q and -w make
// a dark path cheap, and tracepath has no equivalent for either.
func (p *Prober) SelectTraceTool() (string, bool) {
	if p.Look == nil {
		return "", false
	}
	for _, tool := range []string{network.TracerouteTool, network.TracepathTool} {
		if _, err := p.Look.Look(tool); err == nil {
			return tool, true
		}
	}
	return "", false
}

// TraceTimeout is the wall-clock budget for a trace, in seconds.
//
// The two tools differ by nearly 3x at their defaults, which is why this is a
// function of the tool and not a constant. traceroute probes in batches of -N, so
// a fully dark path costs one batch of waiting rather than hops*queries*wait;
// tracepath probes serially and has no -w, so its worst case scales with hop count.
//
// Exported because the collector quotes the number to the operator before the
// trace starts -- a check that may sit silent for 25 seconds should say so.
func TraceTimeout(tool string, cfg network.TracerouteProbes) float64 {
	if tool == network.TracerouteTool {
		batches := math.Ceil(float64(cfg.MaxHops*cfg.Queries) / traceBatch)
		return batches*float64(cfg.WaitS)*4 + 5
	}
	return float64(cfg.MaxHops)*1.5 + 5
}

// Trace walks the path to a target with whichever tool the host has.
//
// Always a TraceResult. A trace that was killed by its own wall clock still
// carries the hops it collected, and those hops are the diagnostic -- "the path
// goes dark after hop 6" is the answer, and it is only visible in a partial
// result. Error is set alongside them rather than instead of them.
func (p *Prober) Trace(ctx context.Context, target string, cfg network.TracerouteProbes) network.TraceResult {
	tool, ok := p.SelectTraceTool()
	if !ok {
		return network.TraceResult{
			Target:  target,
			Hops:    []network.TraceHop{},
			MaxHops: int(cfg.MaxHops),
			Error:   errText(MissingTraceTools),
		}
	}

	budget := TraceTimeout(tool, cfg)
	ctx, cancel := withTimeout(ctx, budget)
	defer cancel()

	res, err := p.Run.Run(ctx, tool, traceArgs(tool, target, cfg)...)

	var wallClock *string
	switch {
	case cmdrun.IsTimeout(err):
		// Killed at the wall clock. res still holds what the tool wrote before it
		// died, so the parse below runs on it and the hops survive.
		wallClock = errText(fmt.Sprintf("%s hit the %gs wall clock (partial)", tool, budget))
	case err != nil:
		// Look() can hand back a dangling symlink, and a binary can vanish between
		// the lookup and the spawn. Either way there is no output to parse.
		return network.TraceResult{
			Target:  target,
			Tool:    tool,
			Hops:    []network.TraceHop{},
			MaxHops: int(cfg.MaxHops),
			Error:   errText(fmt.Sprintf("%s unavailable: %s", tool, err)),
		}
	}

	// stderr merged into stdout, matching v1's `stderr=STDOUT`: both tools write
	// "Name or service not known" to stderr and the hops to stdout, and the parser
	// pulls the tool error out of the same text it pulls hops from.
	result := parseTrace(tool, target, res.Combined(), int(cfg.MaxHops))
	if wallClock != nil {
		// The wall clock outranks whatever the tool said on its way out: it is the
		// reason the trace is short, and the reason is what the operator needs.
		result.Error = wallClock
	}
	return result
}

func traceArgs(tool, target string, cfg network.TracerouteProbes) []string {
	if tool == network.TracerouteTool {
		return []string{
			// -n: rDNS doubles the runtime, and DNS may be the broken thing.
			"-n",
			// -q 1 by default: where the path stops, not per-hop jitter.
			"-q", strconv.FormatInt(cfg.Queries, 10),
			// -w 1 by default: the 5s stock wait makes a black-holed path take
			// minutes.
			"-w", strconv.FormatInt(cfg.WaitS, 10),
			"-N", strconv.Itoa(traceBatch),
			"-m", strconv.FormatInt(cfg.MaxHops, 10),
			target,
		}
	}
	// -4 because the rest of the domain is IPv4-only -- PrimaryIPv4 and the
	// gateway from /proc/net/route both are -- so a v6 trace would be measuring a
	// path nothing else here reports on. tracepath has no -q or -w to pass through.
	return []string{"-4", "-n", "-m", strconv.FormatInt(cfg.MaxHops, 10), target}
}

func parseTrace(tool, target, output string, maxHops int) network.TraceResult {
	if tool == network.TracerouteTool {
		return network.ParseTracerouteOutput(target, output, maxHops)
	}
	return network.ParseTracepathOutput(target, output, maxHops)
}
