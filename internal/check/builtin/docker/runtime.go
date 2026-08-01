package docker

import (
	"context"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/container"
)

// runtimeBudget covers one `docker version`. Generous for a question answered over
// a unix socket, because the case worth waiting for is the one where the daemon is
// starting: a two-second cutoff would report a wedged daemon on a host that was
// merely rebooting, and that is the answer an operator acts on wrongly.
const runtimeBudget = 10 * time.Second

type runtimeCheck struct{ runtime Runtime }

func (runtimeCheck) Spec() check.Spec {
	return check.Spec{
		ID:    RuntimeID,
		Title: "Container runtime",
		// No NeedsBins. The runner's presence gate would report unavailable for a
		// missing docker before this ever ran, which is exactly the answer this
		// check exists to give in its own words -- and would give it without the
		// DOCKER_HOST line that explains most of the surprising cases.
		Domain:    "docker",
		Budget:    runtimeBudget,
		InDefault: true,
	}
}

func (c runtimeCheck) Run(ctx context.Context, in check.Input) check.Result {
	rt := c.runtime(ctx)
	status := statusFor(rt)
	res := check.Result{
		Status:  status,
		Summary: rt.Reason,
		Data:    rt,
		Metrics: []check.Metric{daemonUp(rt, boolValue(rt.Available))},
	}

	// No step on a host with nothing installed. There is no runtime to narrate, and
	// the summary has already said so -- a step repeating it would put a line in
	// steps[] on every database in the fleet.
	if rt.Found() {
		in.Progress.Emit(check.Event{Text: rt.Reason, Status: status})
	}
	return res
}

// statusFor grades the resolved runtime.
//
// crit for an installed CLI whose daemon will not answer, and this is the one
// place in the domain that says so. Installing a container runtime is a deliberate
// act; a host that has one and cannot use it will not start the containers someone
// put on it, and reporting that as unavailable would keep it out of the exit code
// on every run without --strict.
//
// Nothing installed is unavailable rather than a fault: a host with no container
// runtime is a host with no container runtime, and a fleet where half the machines
// are databases would be half red on a verdict that means nothing about them.
func statusFor(rt container.Runtime) check.Status {
	switch {
	case !rt.Found():
		return check.StatusUnavailable
	case !rt.Available:
		return check.StatusCrit
	default:
		return check.StatusOK
	}
}

// daemonUp is the one series a fleet alerts on: 1 when a daemon answered, 0
// otherwise, labelled with the runtime so a podman host is distinguishable from a
// docker host once issue #7 lands. A host with nothing installed reports 0 under an
// empty kind rather than reporting nothing, because a series that disappears looks
// the same as a scrape that failed.
func daemonUp(rt container.Runtime, value float64) check.Metric {
	return gauge(
		DaemonUpMetric, value, "bool",
		map[string]string{"runtime": string(rt.Kind)},
		"1 when the container daemon answered",
	)
}

func boolValue(b bool) float64 {
	if b {
		return 1
	}
	return 0
}
