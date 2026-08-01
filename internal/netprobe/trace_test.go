package netprobe

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// The default traceroute section, so a test that is not about configuration
// does not have to invent one.
var traceCfg = network.DefaultProbes().Traceroute

const tracerouteOutput = `traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms
 2  10.0.0.1  8.114 ms
 3  * * *
 4  8.8.8.8  12.443 ms
`

const tracepathOutput = ` 1:  192.168.1.1                                           0.687ms
 2:  10.0.0.1                                              8.114ms
 3:  no reply
 4:  8.8.8.8                                              12.443ms reached
     Resume: pmtu 1500 hops 4 back 4
`

// traceArgvFor spells the argv once, so the test that stages a response and the
// test that asserts on the command cannot drift apart.
func traceArgvFor(tool, target string) []string {
	return traceArgs(tool, target, traceCfg)
}

func proberWithTraceTools(t *testing.T, installed ...string) (*Prober, *cmdrun.Fake) {
	t.Helper()
	p, fake := newTestProber(t)
	p.Look = cmdrun.NewFakeLooker(installed...)
	return p, fake
}

// traceroute first: -q and -w make a dark path cheap, and tracepath has neither.
func TestSelectTraceToolPrefersTraceroute(t *testing.T) {
	t.Parallel()
	p, _ := proberWithTraceTools(t, "tracepath", "traceroute")
	tool, ok := p.SelectTraceTool()
	if !ok || tool != network.TracerouteTool {
		t.Fatalf("tool = %q, ok = %v", tool, ok)
	}
}

func TestSelectTraceToolFallsBackToTracepath(t *testing.T) {
	t.Parallel()
	p, _ := proberWithTraceTools(t, "tracepath")
	tool, ok := p.SelectTraceTool()
	if !ok || tool != network.TracepathTool {
		t.Fatalf("tool = %q, ok = %v", tool, ok)
	}
}

func TestSelectTraceToolReportsNeitherInstalled(t *testing.T) {
	t.Parallel()
	p, _ := proberWithTraceTools(t)
	if tool, ok := p.SelectTraceTool(); ok {
		t.Fatalf("found %q on a host with neither", tool)
	}
}

// A Prober assembled by hand with no Looker must say "no tool", not panic: the
// zero value reaches this path in any test that stages only a runner.
func TestSelectTraceToolWithNoLookerSaysNo(t *testing.T) {
	t.Parallel()
	var p Prober
	if _, ok := p.SelectTraceTool(); ok {
		t.Fatal("a Prober with no Looker claimed to have found a trace tool")
	}
}

// The two tools differ by nearly 3x at their defaults, which is the reason this
// is a function of the tool rather than one constant for both.
func TestTraceTimeoutIsPerToolAndFitsTheBatching(t *testing.T) {
	t.Parallel()
	// 15 hops * 1 query = 15 probes, one batch of 16, * 1s wait * 4 + 5.
	if got := TraceTimeout(network.TracerouteTool, traceCfg); got != 9 {
		t.Errorf("traceroute budget = %vs, want 9", got)
	}
	// tracepath probes serially and has no -w, so its cost scales with hops.
	if got := TraceTimeout(network.TracepathTool, traceCfg); got != 27.5 {
		t.Errorf("tracepath budget = %vs, want 27.5", got)
	}
	// A second batch costs a second full wait: 30 hops * 3 queries = 90 probes,
	// six batches. A budget that ignored -N would kill this trace mid-path.
	wide := network.TracerouteProbes{MaxHops: 30, WaitS: 2, Queries: 3}
	if got := TraceTimeout(network.TracerouteTool, wide); got != 6*2*4+5 {
		t.Errorf("wide budget = %vs", got)
	}
}

func TestTraceWalksThePathWithTraceroute(t *testing.T) {
	t.Parallel()
	p, fake := proberWithTraceTools(t, "traceroute")
	fake.Stdout(tracerouteOutput, "traceroute", traceArgvFor("traceroute", "8.8.8.8")...)

	res := p.Trace(context.Background(), "8.8.8.8", traceCfg)
	if res.Error != nil {
		t.Fatalf("error = %q", *res.Error)
	}
	if res.Tool != network.TracerouteTool {
		t.Errorf("tool = %q", res.Tool)
	}
	if len(res.Hops) != 4 {
		t.Fatalf("got %d hops: %+v", len(res.Hops), res.Hops)
	}
	if !res.Reached {
		t.Error("the trace reached 8.8.8.8 and did not say so")
	}
	if res.MaxHops != int(traceCfg.MaxHops) {
		t.Errorf("max hops = %d", res.MaxHops)
	}
}

func TestTraceWalksThePathWithTracepath(t *testing.T) {
	t.Parallel()
	p, fake := proberWithTraceTools(t, "tracepath")
	fake.Stdout(tracepathOutput, "tracepath", traceArgvFor("tracepath", "8.8.8.8")...)

	res := p.Trace(context.Background(), "8.8.8.8", traceCfg)
	if res.Error != nil {
		t.Fatalf("error = %q", *res.Error)
	}
	if res.Tool != network.TracepathTool {
		t.Errorf("tool = %q", res.Tool)
	}
	if len(res.Hops) != 4 {
		t.Fatalf("got %d hops: %+v", len(res.Hops), res.Hops)
	}
	if !res.Reached {
		t.Error("the trace reached 8.8.8.8 and did not say so")
	}
}

// The argv is what a support call compares against a hand-run command, and the
// -N the wall clock was computed from has to be on it.
func TestTraceArgsMatchWhatTheBudgetAssumes(t *testing.T) {
	t.Parallel()
	got := strings.Join(traceArgs(network.TracerouteTool, "8.8.8.8", traceCfg), " ")
	if got != "-n -q 1 -w 1 -N 16 -m 15 8.8.8.8" {
		t.Errorf("traceroute argv = %q", got)
	}
	got = strings.Join(traceArgs(network.TracepathTool, "8.8.8.8", traceCfg), " ")
	if got != "-4 -n -m 15 8.8.8.8" {
		t.Errorf("tracepath argv = %q", got)
	}
}

// "The path goes dark after hop 2" is the answer, and it only exists in the
// hops the tool wrote before its wall clock killed it.
func TestTraceKeepsThePartialPathItWasKilledHolding(t *testing.T) {
	t.Parallel()
	p, fake := proberWithTraceTools(t, "traceroute")
	partial := `traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms
 2  10.0.0.1  8.114 ms
 3  * * *
`
	fake.Partial(
		cmdrun.Result{Stdout: partial},
		context.DeadlineExceeded,
		"traceroute", traceArgvFor("traceroute", "8.8.8.8")...,
	)

	res := p.Trace(context.Background(), "8.8.8.8", traceCfg)
	if len(res.Hops) != 3 {
		t.Fatalf("got %d hops, want the ones collected before the kill", len(res.Hops))
	}
	if res.Error == nil {
		t.Fatal("no error on a trace that hit its wall clock")
	}
	if !strings.Contains(*res.Error, "9s wall clock") {
		t.Errorf("error = %q, want the budget it hit", *res.Error)
	}
	if last, ok := res.LastRespondingHop(); !ok || last != 2 {
		t.Errorf("last responding hop = %d (%v), want 2", last, ok)
	}
}

// The wall clock outranks whatever the tool said on the way out: it is the
// reason the trace is short, and that is what the operator needs to read.
func TestTheWallClockOutranksTheToolsOwnComplaint(t *testing.T) {
	t.Parallel()
	p, fake := proberWithTraceTools(t, "traceroute")
	fake.Partial(
		cmdrun.Result{Stdout: "traceroute: something it managed to say\n"},
		context.DeadlineExceeded,
		"traceroute", traceArgvFor("traceroute", "8.8.8.8")...,
	)

	res := p.Trace(context.Background(), "8.8.8.8", traceCfg)
	if res.Error == nil || !strings.Contains(*res.Error, "wall clock") {
		t.Fatalf("error = %v, want the wall clock rather than the tool's line", res.Error)
	}
}

// Look() can hand back a dangling symlink, and a binary can vanish between the
// lookup and the spawn. There is no output to parse, and the result still has
// to be a well-formed document.
func TestTraceReportsASpawnThatFailedAfterTheLookupSucceeded(t *testing.T) {
	t.Parallel()
	p, fake := proberWithTraceTools(t, "traceroute")
	fake.Fail(errors.New("fork/exec: permission denied"), "traceroute", traceArgvFor("traceroute", "8.8.8.8")...)

	res := p.Trace(context.Background(), "8.8.8.8", traceCfg)
	if res.Error == nil || !strings.Contains(*res.Error, "traceroute unavailable") {
		t.Fatalf("error = %v", res.Error)
	}
	if res.Hops == nil {
		t.Error("a nil slice marshals to null; the wire format wants []")
	}
	if res.Target != "8.8.8.8" || res.Tool != network.TracerouteTool {
		t.Errorf("result = %+v, want the target and tool echoed", res)
	}
}

// "traceroute unavailable" alone sends an operator to a search engine for
// something one apt line solves, so the message names the fix.
func TestTraceNamesTheInstallWhenNeitherToolIsPresent(t *testing.T) {
	t.Parallel()
	p, _ := proberWithTraceTools(t)

	res := p.Trace(context.Background(), "8.8.8.8", traceCfg)
	if res.Error == nil || *res.Error != MissingTraceTools {
		t.Fatalf("error = %v", res.Error)
	}
	if res.Tool != "" {
		t.Errorf("tool = %q, want none: nothing ran", res.Tool)
	}
	if res.Hops == nil {
		t.Error("a nil slice marshals to null; the wire format wants []")
	}
	if res.MaxHops != int(traceCfg.MaxHops) {
		t.Errorf("max hops = %d, want it reported even though nothing ran", res.MaxHops)
	}
}

func TestTraceGivesTheProcessTheBudgetItQuoted(t *testing.T) {
	t.Parallel()
	p, _ := proberWithTraceTools(t, "traceroute")
	var seen time.Duration
	p.Run = runnerFunc(func(ctx context.Context, _ string, _ ...string) (cmdrun.Result, error) {
		if deadline, ok := ctx.Deadline(); ok {
			seen = time.Until(deadline)
		}
		return cmdrun.Result{Stdout: tracerouteOutput}, nil
	})

	p.Trace(context.Background(), "8.8.8.8", traceCfg)
	budget := TraceTimeout(network.TracerouteTool, traceCfg)
	if seen <= 0 || seen > time.Duration(budget)*time.Second {
		t.Fatalf("deadline %v away, want just under %vs", seen, budget)
	}
}
