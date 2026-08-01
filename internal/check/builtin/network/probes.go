package network

import (
	"context"
	"fmt"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// The four checks that run probes.yml's configured targets, one check per layer:
// ping, dns, https, tcp. v1 ran all four as a single "probe set" behind one
// button and tallied them into one line; here each layer is its own check,
// because a report whose ping and HTTPS results share a status cannot tell an
// operator that the path is fine and only the service is down -- which is the
// distinction the whole domain is built around.
//
// Each keeps v1's per-target verdict, from screens/network.py, unchanged.

type ping struct {
	p      *netprobe.Prober
	probes corenet.Probes
}

func (c ping) Spec() check.Spec {
	return check.Spec{
		ID:        PingID,
		Title:     "Ping targets",
		Domain:    "network",
		NeedsBins: []string{"ping"},
		Budget: budgetFor(
			len(c.probes.Ping.Targets), float64(c.probes.Ping.TimeoutS),
		),
		InDefault: true,
	}
}

func (c ping) Run(ctx context.Context, in check.Input) check.Result {
	cfg := c.probes.Ping
	if len(cfg.Targets) == 0 {
		return nothingConfigured("ping targets")
	}

	tally := verdicts{noun: "ping targets"}
	summaries := make([]*corenet.PingSummary, 0, len(cfg.Targets))
	var metrics []check.Metric

	for _, target := range cfg.Targets {
		summary := c.p.Ping(ctx, target, cfg.Count, cfg.IntervalS, cfg.TimeoutS)
		summaries = append(summaries, summary)
		if summary == nil {
			// v1's wording. Both causes are named because the operator's next
			// move differs: install iputils-ping, or find out why a 60-packet
			// run did not finish inside its timeout.
			tally.record(in.Progress, check.StatusCrit,
				fmt.Sprintf("ping %s: no usable output (binary missing or timed out)", target))
			continue
		}

		labels := map[string]string{"target": target}
		metrics = append(
			metrics,
			gauge(LossPctMetric, summary.LossPct, "%", labels, "ping packet loss"),
			gauge(RTTAvgMetric, summary.RTTAvgMS, "ms", labels, "ping mean round-trip time"),
			// mdev under its conventional name. Jitter is what makes a link that
			// averages 20ms unusable for anything interactive, and an average
			// alone hides it completely.
			gauge(JitterMetric, summary.JitterMS(), "ms", labels, "ping round-trip jitter (mdev)"),
		)

		// v1's three tiers: total loss is a failure, any loss is a warning. A
		// flaky link and a dead one need different responses, and collapsing them
		// is how intermittent packet loss goes unnoticed until it is total.
		status := check.StatusOK
		switch {
		case summary.LossPct >= 100.0:
			status = check.StatusCrit
		case summary.LossPct > 0:
			status = check.StatusWarn
		}
		tally.record(in.Progress, status, fmt.Sprintf(
			"ping %s  %d/%d  loss %.0f%%  avg %.1fms  jitter %.1fms",
			target, summary.Received, summary.Sent,
			summary.LossPct, summary.RTTAvgMS, summary.JitterMS(),
		))
	}

	res := tally.result()
	res.Data = summaries
	res.Metrics = metrics
	return res
}

type dns struct {
	p      *netprobe.Prober
	probes corenet.Probes
}

func (c dns) Spec() check.Spec {
	return check.Spec{
		ID:     DNSID,
		Title:  "DNS lookups",
		Domain: "network",
		// No binary: this is getaddrinfo through the system resolver, the same
		// path the applications on the host use. Shelling out to dig would test
		// a different resolver than the one that matters.
		Budget:    budgetFor(len(c.probes.DNS.Names), c.probes.DNS.TimeoutS),
		InDefault: true,
	}
}

func (c dns) Run(ctx context.Context, in check.Input) check.Result {
	cfg := c.probes.DNS
	if len(cfg.Names) == 0 {
		return nothingConfigured("dns names")
	}

	tally := verdicts{noun: "names"}
	results := make([]corenet.DNSResult, 0, len(cfg.Names))
	var metrics []check.Metric

	for _, name := range cfg.Names {
		result := c.p.DNS(ctx, name, cfg.TimeoutS)
		results = append(results, result)
		metrics = append(metrics, gauge(DNSLatMetric, result.LatencyMS, "ms",
			map[string]string{"name": name}, "time to resolve a name"))

		if !result.OK {
			tally.record(in.Progress, check.StatusCrit, fmt.Sprintf(
				"DNS %s: %s  (%.1fms)", name, orText(result.Error, "lookup failed"), result.LatencyMS,
			))
			continue
		}
		tally.record(in.Progress, check.StatusOK, fmt.Sprintf(
			"DNS %s → %s  (%.1fms)", name, joinComma(result.Addresses), result.LatencyMS,
		))
	}

	res := tally.result()
	res.Data = results
	res.Metrics = metrics
	return res
}

type https struct {
	p      *netprobe.Prober
	probes corenet.Probes
}

func (c https) Spec() check.Spec {
	return check.Spec{
		ID:        HTTPSID,
		Title:     "HTTPS endpoints",
		Domain:    "network",
		NeedsBins: []string{"curl"},
		Budget:    budgetFor(len(c.probes.HTTP.URLs), float64(c.probes.HTTP.TimeoutS)),
		InDefault: true,
	}
}

func (c https) Run(ctx context.Context, in check.Input) check.Result {
	cfg := c.probes.HTTP
	if len(cfg.URLs) == 0 {
		return nothingConfigured("http urls")
	}

	tally := verdicts{noun: "urls"}
	probes := make([]corenet.CurlProbe, 0, len(cfg.URLs))
	var metrics []check.Metric

	for _, url := range cfg.URLs {
		probe := c.p.Curl(ctx, url, cfg.TimeoutS, cfg.MaxRedirects)
		probes = append(probes, probe)

		if probe.Error != nil {
			// No exchange happened: DNS, the connect, or the handshake. Nothing
			// to measure, so nothing is recorded -- a zero on the timing series
			// would read as an instant response.
			tally.record(in.Progress, check.StatusCrit,
				fmt.Sprintf("curl %s: %s", url, *probe.Error))
			continue
		}

		labels := map[string]string{"url": url}
		metrics = append(
			metrics,
			gauge(HTTPCodeMetric, float64(probe.HTTPCode), "code", labels, "HTTP status code returned"),
			gauge(HTTPTotalMetric, probe.TimeTotalS*1000, "ms", labels, "total time for the HTTP exchange"),
		)

		// v1's rule: a 4xx or 5xx is a warn, not a fail, because the server
		// answered -- which means every layer underneath it works. Grading it
		// crit would make an endpoint that returns 401 by design look like an
		// outage.
		status := check.StatusOK
		if !probe.OK {
			status = check.StatusWarn
		}
		tally.record(in.Progress, status, fmt.Sprintf(
			"curl %s — HTTP %d in %.1fms (dns %.1fms · connect %.1fms · tls %.1fms · ttfb %.1fms) %dB",
			url, probe.HTTPCode, probe.TimeTotalS*1000,
			probe.TimeNamelookupS*1000, probe.TimeConnectS*1000,
			probe.TimeAppconnectS*1000, probe.TimeStarttransferS*1000,
			probe.SizeDownloadBytes,
		))
	}

	res := tally.result()
	res.Data = probes
	res.Metrics = metrics
	return res
}

type tcp struct {
	p      *netprobe.Prober
	probes corenet.Probes
}

func (c tcp) Spec() check.Spec {
	return check.Spec{
		ID:     TCPID,
		Title:  "TCP reachability",
		Domain: "network",
		// No binary: a socket connect, the same as v1's socket.connect_ex. There
		// is no nc invocation to find in the process table, which is why the
		// steps below spell out what was attempted.
		Budget:    budgetFor(len(c.probes.TCP.Targets), c.probes.TCP.TimeoutS),
		InDefault: true,
	}
}

func (c tcp) Run(ctx context.Context, in check.Input) check.Result {
	cfg := c.probes.TCP
	if len(cfg.Targets) == 0 {
		return nothingConfigured("tcp targets")
	}

	tally := verdicts{noun: "targets"}
	checks := make([]corenet.PortCheck, 0, len(cfg.Targets))
	var metrics []check.Metric

	for _, target := range cfg.Targets {
		port := c.p.Port(ctx, target, cfg.TimeoutS)
		checks = append(checks, port)
		metrics = append(metrics, gauge(TCPConnMetric, port.LatencyMS, "ms",
			map[string]string{"target": target.String()}, "time to complete or refuse a TCP connect"))

		line := fmt.Sprintf("tcp %s → %s in %.0fms", target, port.State, port.LatencyMS)
		if port.Error != nil {
			line += ": " + *port.Error
		}
		tally.record(in.Progress, portStatus(port.State), line)
	}

	res := tally.result()
	res.Data = checks
	res.Metrics = metrics
	return res
}

// portStatus is v1's _PORT_STATUS table: open passes, refused warns, everything
// else fails.
//
// Refused is a warning because it is an answer. Something on the other end sent
// a RST, which means the route, the firewall and the host are all working and
// only the service is not listening -- a materially better position than a
// timeout, where nothing came back and the cause could be any of the four.
func portStatus(state corenet.PortState) check.Status {
	switch state {
	case corenet.PortOpen:
		return check.StatusOK
	case corenet.PortRefused:
		return check.StatusWarn
	default:
		return check.StatusCrit
	}
}

// joinComma renders a lookup's answer. An empty list is a real and confusing DNS
// state -- resolved, no addresses -- and has to read as itself rather than as an
// empty gap after the arrow.
func joinComma(items []string) string {
	if len(items) == 0 {
		return "no addresses"
	}
	return strings.Join(items, ", ")
}
