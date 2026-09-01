// Package network registers the network domain's checks.
//
// The split is the same one the disk domain makes: internal/core/network parses
// and internal/netprobe does the I/O, and what is left here is deciding what a
// probe's result means. What is different is where the meaning comes from.
//
// Every other domain grades against internal/threshold, which holds v1's nine
// scattered severities in one place. There is no network rule in it, and that is
// not an oversight -- v1 had no network severity ladder either. Its network
// screen graded pass/warn/fail from the probe's own shape: a 4xx is a warn
// because the server answered, a refused port is a warn because something is
// listening on the path to it, a dark traceroute past hop 6 is a warn because
// transit routers rate-limit ICMP. Those are judgements about what a result
// means, not bounds on a number, and there is no knob an operator would set.
// Inventing thresholds here would smuggle in policy that v1 never had and that
// nothing in the fleet was tuned against, so these checks grade boolean and
// trips[] stays empty for the whole domain.
//
// The wording of each verdict is v1's, from screens/network.py, minus the status
// markers: an Event carries its Status, and a "✗" baked into the text would be
// drawn twice by anything that renders one.
package network

import (
	"fmt"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// The network domain's check ids. Public API: they appear in checks[], in
// --check selectors, and in whatever Ansible an operator writes against them.
const (
	LadderID     check.ID = "network.ladder"
	InterfaceID  check.ID = "network.interface"
	ResolverID   check.ID = "network.resolver"
	PingID       check.ID = "network.ping"
	DNSID        check.ID = "network.dns"
	HTTPSID      check.ID = "network.https"
	TCPID        check.ID = "network.tcp"
	SocketsID    check.ID = "network.sockets"
	TracerouteID check.ID = "network.traceroute"
)

// Metric names, likewise public: these are what a dashboard and --prom pin on.
const (
	RxBytesMetric   = "network.rx_bytes"
	TxBytesMetric   = "network.tx_bytes"
	LossPctMetric   = "network.ping_loss_pct"
	RTTAvgMetric    = "network.ping_rtt_avg_ms"
	JitterMetric    = "network.ping_jitter_ms"
	DNSLatMetric    = "network.dns_latency_ms"
	HTTPCodeMetric  = "network.http_code"
	HTTPTotalMetric = "network.http_time_total_ms"
	TCPConnMetric   = "network.tcp_connect_ms"
	ListenersMetric = "network.listening_ports"
	TraceHopsMetric = "network.trace_hops"
)

// slack is what every budget below adds on top of the probes' own timeouts.
//
// The probe timeouts bound the waiting; this covers the spawning, the parsing
// and a loaded host's scheduling between them. Without it a run whose probes all
// hit their own budgets exactly would be cut off by the check budget instead,
// turning a set of honest per-target timeouts into one opaque cancellation.
const slack = 5 * time.Second

// Checks returns the network domain's checks, wired to one prober and one
// resolved probes.yml.
//
// Both are passed in for the same reason the disk domain takes its runner: a
// test has to be able to stage the host, and the probes are configuration that
// internal/resolve has already read once for the whole run. A check that read
// probes.yml itself could grade against a file the report does not describe.
func Checks(p *netprobe.Prober, probes corenet.Probes) []check.Check {
	return []check.Check{
		ladder{p: p, probes: probes},
		iface{p: p},
		resolver{p: p},
		ping{p: p, probes: probes},
		dns{p: p, probes: probes},
		https{p: p, probes: probes},
		tcp{p: p, probes: probes},
		sockets{p: p},
		traceroute{p: p, probes: probes},
	}
}

// verdicts folds a run of per-target probes into one status and one summary.
//
// v1 ran its configured probes as a set and tallied OK/WARN/BAD into a single
// line, which is the same question a check has to answer: one status for a
// document, with the per-target detail in steps[]. The wording is that tally's,
// from _probe_set_verdict.
type verdicts struct {
	// noun names what was probed, plural, for the summary line: "ping targets",
	// "names", "urls". v1 said "configured probes" for all of them because they
	// shared one pane; here each kind is its own check and can say which.
	noun string

	ok, warn, bad int
}

// record streams one target's verdict and counts it.
func (v *verdicts) record(to check.Emitter, status check.Status, text string) {
	switch status {
	case check.StatusOK:
		v.ok++
	case check.StatusWarn:
		v.warn++
	default:
		v.bad++
	}
	to.Emit(check.Event{Text: text, Status: status})
}

func (v verdicts) ran() int { return v.ok + v.warn + v.bad }

// result is the check's own answer: the worst thing that happened, said once.
func (v verdicts) result() check.Result {
	switch {
	case v.bad > 0:
		return check.Result{
			Status:  check.StatusCrit,
			Summary: fmt.Sprintf("%d of %d %s failed", v.bad, v.ran(), v.noun),
		}
	case v.warn > 0:
		return check.Result{
			Status:  check.StatusWarn,
			Summary: fmt.Sprintf("%d %s ran, %d with warnings", v.ran(), v.noun, v.warn),
		}
	default:
		return check.Result{
			Status:  check.StatusOK,
			Summary: fmt.Sprintf("all %d %s passed", v.ran(), v.noun),
		}
	}
}

// nothingConfigured is the answer when probes.yml asked for no targets of this
// kind.
//
// skipped, not ok: an operator who emptied a list got what they asked for, and
// reporting ok would put a green row on a check that measured nothing. Reachable
// only by an explicit empty list, since the defaults ship targets for every kind.
func nothingConfigured(noun string) check.Result {
	return check.Result{
		Status:  check.StatusSkipped,
		Summary: "probes.yml configures no " + noun,
	}
}

func gauge(name string, value float64, unit string, labels map[string]string, help string) check.Metric {
	return check.Metric{
		Name:   name,
		Value:  value,
		Unit:   unit,
		Labels: labels,
		Kind:   check.Gauge,
		Help:   help,
	}
}

func counter(name string, value float64, unit string, labels map[string]string, help string) check.Metric {
	m := gauge(name, value, unit, labels, help)
	m.Kind = check.Counter
	return m
}

// budgetFor is the wall clock for a check that runs one probe per target in
// sequence.
//
// Derived from the configured timeouts rather than a constant, so an operator
// who raises a probe's timeout is not silently cut off by a check budget that
// did not move with it -- which would read as the tool failing rather than the
// probe taking the time it was told it could.
func budgetFor(targets int, perTargetS float64) time.Duration {
	return time.Duration(float64(targets)*perTargetS*float64(time.Second)) + slack
}
