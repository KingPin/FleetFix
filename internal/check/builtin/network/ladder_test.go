package network

import (
	"context"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
)

// ladderPingArgv is the argv the ladder's two ping rungs build. Deliberately not
// pingArgv: the ladder pings three packets, not the standalone probe's ten, and a
// test that shared one helper would stop noticing if they converged.
func ladderPingArgv(target string) []string {
	return []string{"-c", "3", "-i", "0.2", target}
}

// healthyLadder stages every rung's answer on an otherwise-default host.
func healthyLadder(t *testing.T) *host {
	t.Helper()
	h := newHost(t)
	h.run.Stdout(pingClean, "ping", ladderPingArgv("192.168.1.1")...)
	h.run.Stdout(pingClean, "ping", ladderPingArgv("8.8.8.8")...)
	h.run.Stdout(curl200, "curl", curlArgv("https://github.com")...)
	return h
}

func TestLadderClimbsEveryRung(t *testing.T) {
	t.Parallel()
	h := healthyLadder(t)

	res, streamed := h.runID(t, LadderID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	want := "all 5 rungs up — link, gateway, internet, dns and https all answered"
	if res.Summary != want {
		t.Errorf("summary = %q", res.Summary)
	}
	if len(res.Steps) != 5 {
		t.Fatalf("got %d steps, want one per rung: %+v", len(res.Steps), res.Steps)
	}
	// Streamed as they land, not flushed at the end. A ladder is up to half a
	// minute of subprocesses and the whole reason core calls OnRung is that a
	// caller can render it climbing.
	if len(streamed) != len(res.Steps) {
		t.Errorf("streamed %d events for %d steps", len(streamed), len(res.Steps))
	}
	if res.Steps[0].Text != "link state: eth0 10.0.0.5 via 192.168.1.1 (up)" {
		t.Errorf("first step = %q", res.Steps[0].Text)
	}
	// Bottom-up, and the order is the diagnosis: the gateway is only meaningful
	// once the link is up, and the internet only once the gateway answers.
	for i, want := range []string{"link state", "default gateway", "internet", "dns", "https"} {
		if !strings.HasPrefix(res.Steps[i].Text, want) {
			t.Errorf("rung %d = %q, want the %s rung", i, res.Steps[i].Text, want)
		}
	}
}

// The whole point of the check: not "curl failed" but which layer is at fault.
func TestLadderNamesTheLowestBrokenRung(t *testing.T) {
	t.Parallel()
	h := healthyLadder(t)
	h.run.Exit(1, pingDead, "", "ping", ladderPingArgv("8.8.8.8")...)

	res, _ := h.runID(t, LadderID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	if !strings.HasPrefix(res.Summary, "internet (8.8.8.8) is the lowest thing broken") {
		t.Errorf("summary = %q", res.Summary)
	}
	// Every rung still ran. Halting at the first failure would report "internet
	// down" without ever finding out that DNS and HTTPS both work, which is what
	// says the failure was ICMP filtering rather than an outage.
	if len(res.Steps) != 5 {
		t.Errorf("got %d steps, want the rungs above the failure to have run too", len(res.Steps))
	}
	if last := res.Steps[4]; last.Status != check.StatusOK {
		t.Errorf("https rung = %q, want it to have run and passed", last.Status)
	}
}

// A rung with nothing to test is skipped, not passed. Counting it as a pass is how
// a box with no default route gets a green line on the gateway.
func TestLadderSkipsARungWithNothingToTest(t *testing.T) {
	t.Parallel()
	h := healthyLadder(t)
	h.prober.Host = hostfs.Host{Proc: fstest.MapFS{}, Sys: fstest.MapFS{}}

	res, _ := h.runID(t, LadderID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	if !strings.HasPrefix(res.Summary, "link state is the lowest thing broken") {
		t.Errorf("summary = %q", res.Summary)
	}
	if got := res.Steps[1].Status; got != check.StatusSkipped {
		t.Errorf("gateway rung = %q, want skipped: %q", got, res.Steps[1].Text)
	}
}

// The runner's budget arrives as a deadline on the context. An expired one means
// nobody is reading the answer, so the rest of the ladder is skipped rather than
// run into a dead context and reported as five failures.
func TestLadderStopsWhenNobodyIsWaiting(t *testing.T) {
	t.Parallel()
	h := healthyLadder(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res := find(t, Checks(h.prober, probes()), LadderID).
		Run(ctx, check.Input{Params: map[string]string{}, Progress: check.Discard}).
		Normalize()

	if res.Status != check.StatusWarn {
		t.Fatalf("status = %q, want warn: %s", res.Status, res.Summary)
	}
	if res.Summary != "the ladder stopped before any rung ran" {
		t.Errorf("summary = %q", res.Summary)
	}
	// Not one subprocess for a run nobody will read.
	if calls := h.run.Calls(); len(calls) != 0 {
		t.Errorf("ran %v after the context was already gone", calls)
	}
}

// Cancelled mid-climb: what ran is reported, and the rungs that never got their
// turn are not counted as failures.
func TestLadderReportsAPartialClimbAsIncomplete(t *testing.T) {
	t.Parallel()
	h := healthyLadder(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Cancel as the first rung lands, which is the shape of a runner whose budget
	// expires mid-check.
	first := true
	in := check.Input{
		Params: map[string]string{},
		Progress: check.EmitterFunc(func(check.Event) {
			if first {
				first = false
				cancel()
			}
		}),
	}
	res := find(t, Checks(h.prober, probes()), LadderID).Run(ctx, in).Normalize()

	if res.Status != check.StatusWarn {
		t.Fatalf("status = %q, want warn: %s", res.Status, res.Summary)
	}
	if res.Summary != "1 of 5 rungs passed, the rest never ran" {
		t.Errorf("summary = %q", res.Summary)
	}
}

// A ladder cut off by its own budget is worse than useless -- it reports rungs as
// broken that were only interrupted -- so the budget has to cover what the five
// rungs can actually cost.
func TestLadderBudgetsForEveryRungItWillRun(t *testing.T) {
	t.Parallel()
	spec := find(t, Checks(nil, probes()), LadderID).Spec()

	// Two ladder pings at 6s, the DNS timeout at 3s, the HTTP timeout at 15s, plus
	// the shared slack.
	want := 35 * time.Second
	if spec.Budget != want {
		t.Errorf("budget = %v, want %v", spec.Budget, want)
	}
	// ping only. Gating the whole ladder on curl would mean a host without it
	// reports nothing at all about its link, which is the rung that matters most.
	if len(spec.NeedsBins) != 1 || spec.NeedsBins[0] != "ping" {
		t.Errorf("needs %v", spec.NeedsBins)
	}
}

// A missing tool is not a broken network. The ladder must not report crit when
// curl is simply absent; the https rung goes unavailable instead.
func TestLadderDoesNotBlameAHostThatHasNoCurl(t *testing.T) {
	t.Parallel()
	h := newHost(t, "ping", "ss", "traceroute")
	h.run.Stdout(pingClean, "ping", ladderPingArgv("192.168.1.1")...)
	h.run.Stdout(pingClean, "ping", ladderPingArgv("8.8.8.8")...)

	res, _ := h.runID(t, LadderID, nil)

	if res.Status != check.StatusWarn {
		t.Fatalf("status = %q, want StatusWarn", res.Status)
	}
	if !strings.Contains(res.Summary, "curl is not installed") {
		t.Errorf("summary = %q, want to contain 'curl is not installed'", res.Summary)
	}
	if res.Steps[4].Status != check.StatusUnavailable {
		t.Errorf("https rung (step 4) status = %q, want StatusUnavailable", res.Steps[4].Status)
	}
}
