package network

import (
	"context"
	"fmt"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// ladder is the check the rest of the domain exists to support: five rungs
// bottom-up, and the name of the lowest one that broke.
//
// The other checks say a probe failed. This one says which layer failed, which
// is the difference between fixing it and guessing -- and it is the check whose
// streaming shape the TUI reuses, since core's Ladder already calls OnRung as
// each rung lands and Input.Progress is exactly that callback with a report on
// the other end instead of a screen.
type ladder struct {
	p      *netprobe.Prober
	probes corenet.Probes
}

func (c ladder) Spec() check.Spec {
	return check.Spec{
		ID:     LadderID,
		Title:  "Connectivity ladder",
		Domain: "network",
		// ping is the only binary: the link and gateway facts are /proc reads,
		// DNS goes through the system resolver, and curl is checked by the rung
		// that runs it rather than gating the whole ladder. Gating on curl would
		// mean a host without it reports nothing at all about its link.
		NeedsBins: []string{"ping"},
		Budget:    c.budget(),
		InDefault: true,
	}
}

// budget is the sum of what the five rungs can cost.
//
// Two ping rungs at core's own ladder timeout, plus the DNS and HTTP timeouts
// from probes.yml. The link rung is two /proc reads and costs nothing worth
// counting.
func (c ladder) budget() time.Duration {
	seconds := float64(2*corenet.LadderPingTimeoutS) +
		c.probes.DNS.TimeoutS +
		float64(c.probes.HTTP.TimeoutS)
	return time.Duration(seconds*float64(time.Second)) + slack
}

func (c ladder) Run(ctx context.Context, in check.Input) check.Result {
	res := check.Result{}

	// Asked once, before the walk, and used by both the steps and the verdict: two
	// Look calls could disagree if a package landed mid-run, and a step that said
	// unavailable under a summary that said crit would be worse than either.
	noCurl := !c.hasCurl()

	walk := corenet.Ladder{
		Probes: c.probes,

		// One step per rung, as it lands. A ladder is up to half a minute of
		// subprocesses and the whole point is watching it climb, so a check that
		// only reported at the end would be unusable in the TUI and no more
		// informative in the report.
		OnRung: func(rung corenet.LadderRung) {
			res.Steps = append(res.Steps, check.Event{
				Text:   fmt.Sprintf("%s: %s", rung.Label, rung.Detail),
				Status: rungStatus(rung, noCurl && rung.Name == corenet.RungHTTPS),
			})
			in.Progress.Emit(res.Steps[len(res.Steps)-1])
		},

		// The runner's budget arrives as a deadline on this context, so an
		// expired one means nobody is waiting for the rest. Returning false marks
		// the remainder skipped rather than running probes into a dead context and
		// reporting five failures for one cancellation.
		ShouldContinue: func() bool { return ctx.Err() == nil },

		Net: func() *corenet.Info { return c.p.Network(ctx) },
		Ping: func(target string, count, timeoutS int64) *corenet.PingSummary {
			return c.p.Ping(ctx, target, count, c.probes.Ping.IntervalS, timeoutS)
		},
		DNS: func(name string, timeoutS float64) corenet.DNSResult {
			return c.p.DNS(ctx, name, timeoutS)
		},
		Curl: func(url string, timeoutS, maxRedirects int64) corenet.CurlProbe {
			return c.p.Curl(ctx, url, timeoutS, maxRedirects)
		},
	}

	result := walk.Run()
	res.Data = result

	ran, passed := 0, 0
	for _, rung := range result.Rungs {
		if rung.Skipped {
			continue
		}
		ran++
		if rung.OK {
			passed++
		}
	}

	// v1's _ladder_verdict, in order. The lowest failure outranks everything,
	// including the fact that rungs above it were skipped: every rung above a
	// failure is only meaningful once that layer works.
	switch failure, failed := result.FirstFailure(); {
	case failed && failure.Name == corenet.RungHTTPS && noCurl:
		// A rung that went unasked, not one that failed. curl is deliberately
		// absent from NeedsBins -- gating the whole ladder on it would mean a host
		// without curl reports nothing at all about its link -- and this branch is
		// the cost of that choice. Without it a minimal image grades crit with
		// "https is the lowest thing broken — cmdrun: executable not found: curl",
		// which pages somebody about a host whose network is fine. Found by running
		// the smoke gate against alpine:3.20.
		//
		// Not crit, because nothing is broken; not ok, because a ladder missing its
		// top rung is not a ladder that climbed. Warn is the same verdict this check
		// already gives a run that could not reach every rung.
		//
		// core puts an absent tool and a broken transport in the same rung, and has
		// to: it reproduces v1 byte for byte and is what the differential harness
		// compares. Deciding what a failed rung *means* is this layer's job.
		res.Status = check.StatusWarn
		res.Summary = fmt.Sprintf(
			"%d of %d rungs up; %s could not be tested because curl is not installed on this host",
			passed, len(result.Rungs), failure.Label,
		)
	case failed:
		res.Status = check.StatusCrit
		res.Summary = fmt.Sprintf("%s is the lowest thing broken — %s", failure.Label, failure.Detail)
	case ran == 0:
		res.Status = check.StatusWarn
		res.Summary = "the ladder stopped before any rung ran"
	case ran < len(result.Rungs):
		res.Status = check.StatusWarn
		res.Summary = fmt.Sprintf("%d of %d rungs passed, the rest never ran", ran, len(result.Rungs))
	default:
		res.Status = check.StatusOK
		res.Summary = fmt.Sprintf(
			"all %d rungs up — link, gateway, internet, dns and https all answered", ran,
		)
	}
	return res
}

// rungStatus is the status of one rung's step.
//
// A skipped rung is skipped rather than ok: "no gateway to test" on a host with
// no default route is not a rung that passed, and counting it as one is how a
// box with no path off itself gets a green line.
//
// unaskable is the https rung on a host with no curl. Unavailable rather than
// crit for the same reason the verdict above is warn rather than crit -- and the
// step matters on its own account, because it is the line the TUI draws red.
func rungStatus(rung corenet.LadderRung, unaskable bool) check.Status {
	switch {
	case rung.Skipped:
		return check.StatusSkipped
	case rung.OK:
		return check.StatusOK
	case unaskable:
		return check.StatusUnavailable
	default:
		return check.StatusCrit
	}
}

// hasCurl answers whether the https rung can be asked at all.
//
// A nil Looker means nothing can be looked up rather than everything is present:
// a prober assembled without one has no way to know, and reporting a rung as
// broken on that basis would be the same false page this guard exists to stop.
func (c ladder) hasCurl() bool {
	if c.p.Look == nil {
		return false
	}
	_, err := c.p.Look.Look("curl")
	return err == nil
}
