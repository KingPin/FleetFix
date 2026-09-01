package network

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// A path that answers for three hops and then goes silent, which is the common
// shape: transit routers that rate-limit or drop ICMP TTL-exceeded.
const tracerouteDark = `traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms
 2  10.0.0.1  8.114 ms
 3  203.0.113.9  11.201 ms
 4  *
 5  *
 6  *
`

const tracerouteSilent = `traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets
 1  *
 2  *
`

func traceArgv(target string) []string {
	return []string{"-n", "-q", "1", "-w", "1", "-N", "16", "-m", "15", target}
}

func tracepathArgv(target string) []string {
	return []string{"-4", "-n", "-m", "15", target}
}

func TestTraceReportsThePathItReached(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(tracerouteReached, "traceroute", traceArgv("8.8.8.8")...)

	res, streamed := h.runID(t, TracerouteID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	if res.Summary != "trace 8.8.8.8 — reached in 3 hops (12.4ms)" {
		t.Errorf("summary = %q", res.Summary)
	}
	// The opening line, then one step per hop rendered the way traceroute renders
	// it.
	if len(res.Steps) != 4 {
		t.Fatalf("got %d steps: %+v", len(res.Steps), res.Steps)
	}
	if res.Steps[1].Text != "  1  192.168.1.1  0.7ms" {
		t.Errorf("first hop = %q", res.Steps[1].Text)
	}
	// Said before the trace runs, because this check can sit silent for half a
	// minute and a row that does not say why reads as a hang.
	if want := "traceroute to 8.8.8.8, up to 15 hops (up to 9s)"; streamed[0].Text != want {
		t.Errorf("opening event = %q", streamed[0].Text)
	}
	// The report carries it too: which tool ran, against what target and what
	// wall clock is the first thing to check when a trace says nothing, and a
	// check that built its own steps[] would have dropped this line from it.
	if res.Steps[0].Text != streamed[0].Text {
		t.Errorf("steps[0] = %q, want the opening line the stream carried", res.Steps[0].Text)
	}
	// Streamed once, not once per rendering surface.
	if len(streamed) != len(res.Steps) {
		t.Errorf("streamed %d events for %d steps: %+v", len(streamed), len(res.Steps), streamed)
	}
	if m := metricNamed(t, res, TraceHopsMetric); m.Value != 3 {
		t.Errorf("hops metric = %v", m.Value)
	}
}

// Warn, not fail. A dark path past hop 3 says nothing about whether the
// destination is reachable -- that is what ping answers -- and grading it crit
// would put a red row on most hosts on the internet.
func TestTraceWarnsWhenThePathGoesDark(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(tracerouteDark, "traceroute", traceArgv("8.8.8.8")...)

	res, _ := h.runID(t, TracerouteID, nil)
	if res.Status != check.StatusWarn {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	want := "trace 8.8.8.8 — path goes dark after hop 3 (no reply from hop 4 to 15)"
	if res.Summary != want {
		t.Errorf("summary = %q", res.Summary)
	}
	// A silent hop in the middle is a warning on its own row and never a failure:
	// one router configured not to reply is not a fault.
	// Offset by one: steps[0] is the opening line, so hop N is steps[N].
	if got := res.Steps[4].Status; got != check.StatusWarn {
		t.Errorf("silent hop = %q, want warn", got)
	}
	if got := res.Steps[1].Status; got != check.StatusOK {
		t.Errorf("responding hop = %q", got)
	}
}

// Nothing at all came back, which is a different diagnosis from a path that
// stalls partway and gets its own wording rather than "stalled at hop 1".
func TestTraceFailsWhenNothingOnThePathAnswered(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(tracerouteSilent, "traceroute", traceArgv("8.8.8.8")...)

	res, _ := h.runID(t, TracerouteID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Summary != "trace 8.8.8.8 — nothing answered in 15 hops" {
		t.Errorf("summary = %q", res.Summary)
	}
}

// No hops at all: the tool refused before it started, and its own message is the
// finding.
func TestTraceCarriesTheToolsOwnRefusal(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout("traceroute: unknown host nope.invalid",
		"traceroute", traceArgv("nope.invalid")...)

	res, _ := h.runID(t, TracerouteID, map[string]string{TargetParam: "nope.invalid"})
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Summary != "trace nope.invalid: unknown host nope.invalid" {
		t.Errorf("summary = %q", res.Summary)
	}
}

// The hops a killed trace collected are the diagnostic. Reporting only the
// timeout would throw away the answer the trace had already found.
func TestTraceKeepsThePartialPathItCollectedBeforeTheWallClock(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Partial(cmdrun.Result{Stdout: tracerouteDark}, context.DeadlineExceeded,
		"traceroute", traceArgv("8.8.8.8")...)

	res, _ := h.runID(t, TracerouteID, nil)
	if len(res.Steps) != 8 {
		t.Fatalf("got %d steps, want the opening line, 6 hops and the reason: %+v",
			len(res.Steps), res.Steps)
	}
	last := res.Steps[7]
	if last.Text != "traceroute hit the 9s wall clock (partial)" {
		t.Errorf("reason = %q", last.Text)
	}
	if last.Status != check.StatusWarn {
		t.Errorf("reason status = %q", last.Status)
	}
}

// A hop that answered with an ICMP flag and no timing. The responder and the flag
// are both the finding -- an !H is a router saying the destination is unreachable
// -- and an empty timing column would read as a missing reading rather than a hop
// that had none to give.
func TestTraceRendersAHopThatAnsweredWithoutATiming(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(`traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms
 2  203.0.113.9  !H
`, "traceroute", traceArgv("8.8.8.8")...)

	res, _ := h.runID(t, TracerouteID, nil)
	if res.Steps[2].Text != "  2  203.0.113.9  no rtt  !H" {
		t.Errorf("hop = %q", res.Steps[2].Text)
	}
	// A router answering *for* the destination is not the destination answering,
	// so this is not a trace that reached its target.
	if res.Status != check.StatusWarn {
		t.Errorf("status = %q: %s", res.Status, res.Summary)
	}
}

// Either binary will do, so gating on one name would report unavailable on a host
// that has the other. Which one ran changes the argv and the wall clock.
func TestTraceRunsWhicheverToolTheHostHas(t *testing.T) {
	t.Parallel()
	h := newHost(t, "tracepath")
	h.run.Stdout("1:  192.168.1.1  0.687ms\nResume: pmtu 1500 hops 1 back 1\n",
		"tracepath", tracepathArgv("8.8.8.8")...)

	res, streamed := h.runID(t, TracerouteID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	// tracepath probes serially and has no -w, so its worst case is nearly 3x
	// traceroute's. Quoting traceroute's number here would be a lie by 18 seconds.
	if want := "tracepath to 8.8.8.8, up to 15 hops (up to 27.5s)"; streamed[0].Text != want {
		t.Errorf("opening event = %q", streamed[0].Text)
	}
	if !h.run.Called("tracepath", tracepathArgv("8.8.8.8")...) {
		t.Errorf("ran %v", h.run.Calls())
	}
}

// Unavailable, not error: the host is missing a tool, which is a fact about the
// host and not something that went wrong. The message names the fix, because
// "traceroute unavailable" sends an operator to a search engine for something one
// command solves.
func TestTraceIsUnavailableWithNeitherToolInstalled(t *testing.T) {
	t.Parallel()
	h := newHost(t, "ping", "curl", "ss")

	res, _ := h.runID(t, TracerouteID, nil)
	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Error != netprobe.MissingTraceTools {
		t.Errorf("error = %q", res.Error)
	}
	if len(h.run.Calls()) != 0 {
		t.Errorf("spawned %v with no tool to spawn", h.run.Calls())
	}
}

// Defaulted rather than required, so `--check network.traceroute` with no
// arguments traces the target the ladder's internet rung pings -- which is the
// trace an operator wants right after that rung fails.
func TestTraceDefaultsToTheLaddersInternetTarget(t *testing.T) {
	t.Parallel()
	custom := probes()
	custom.Ladder.InternetTarget = "9.9.9.9"

	spec := find(t, Checks(nil, custom), TracerouteID).Spec()
	if len(spec.Params) != 1 || spec.Params[0].Name != TargetParam {
		t.Fatalf("params = %+v", spec.Params)
	}
	if spec.Params[0].Default != "9.9.9.9" {
		t.Errorf("default target = %q", spec.Params[0].Default)
	}
	if spec.Params[0].Required {
		t.Error("target is required, so the check cannot run unattended")
	}
}

// An empty argument is a question that was never asked, so it is skipped rather
// than errored. Only reachable by passing --param target= with nothing after it.
func TestTraceWithAnEmptyTargetIsSkipped(t *testing.T) {
	t.Parallel()
	h := newHost(t)

	res, _ := h.runID(t, TracerouteID, map[string]string{TargetParam: ""})
	if res.Status != check.StatusSkipped {
		t.Fatalf("status = %q", res.Status)
	}
	if len(h.run.Calls()) != 0 {
		t.Errorf("spawned %v for a target that was not given", h.run.Calls())
	}
}

// Which tool will run is not known until the check does, so budgeting for
// traceroute on a host that only has tracepath would cut the trace off at a third
// of the time tracepath needs.
func TestTraceBudgetsForTheSlowerOfTheTwoTools(t *testing.T) {
	t.Parallel()
	spec := find(t, Checks(nil, probes()), TracerouteID).Spec()

	if want := 32500 * time.Millisecond; spec.Budget != want {
		t.Errorf("budget = %v, want %v (tracepath's 27.5s plus slack)", spec.Budget, want)
	}
	if len(spec.NeedsBins) != 0 {
		t.Errorf("needs %v, which would report unavailable on a host with the other tool", spec.NeedsBins)
	}
}

// The trace is a diagnostic, and a diagnostic that reads as a verdict is worse
// than none: a hop line already carries its own status.
func TestTraceStepsAreNotMarkedUp(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(tracerouteDark, "traceroute", traceArgv("8.8.8.8")...)

	_, streamed := h.runID(t, TracerouteID, nil)
	for _, e := range streamed {
		if strings.ContainsAny(e.Text, "✓✗!·⋯") {
			t.Errorf("step carries a marker: %q", e.Text)
		}
	}
}
