package network

import "fmt"

// The ladder walks the network stack one layer at a time and names the layer that
// broke.
//
// "curl failed" is not a diagnosis. Five rungs bottom-up -- link, gateway,
// internet, DNS, HTTPS -- so the answer is "DNS is the problem", which is the
// difference between fixing it and guessing.
//
// Every rung runs, even after one fails. Halting at the first failure would
// confidently report "gateway down" on any cloud host whose gateway drops ICMP --
// a box that is in fact perfectly healthy. FirstFailure still names the lowest
// failure, because that is where to start looking; the rest of the rungs are what
// tell you whether that failure actually mattered.
//
// Every sub-probe is injected, so a test exercises the whole ladder without
// touching the network and ShouldContinue stays a plain func -- the worker-liveness
// question belongs to whatever is driving the ladder, not to the ladder.

// Rung names. Stable: they are what a caller keys a rung off, and at M3 they become
// part of the report's steps[].
const (
	RungLink     = "link"
	RungGateway  = "gateway"
	RungInternet = "internet"
	RungDNS      = "dns"
	RungHTTPS    = "https"
)

// Ladder pings are deliberately shorter than the standalone ping probe: this asks
// "does anything come back", not "how stable is this link over time".
const (
	ladderPingCount    int64 = 3
	ladderPingTimeoutS int64 = 6
)

// LadderRung is one layer's verdict.
//
// Skipped separates "this did not pass" from "this was never asked": a rung with
// nothing to test, and every rung after a cancellation. Neither counts against the
// ladder, which is why OK and FirstFailure both step over them.
type LadderRung struct {
	Name    string `json:"name"`
	Label   string `json:"label"`
	OK      bool   `json:"ok"`
	Detail  string `json:"detail"`
	Skipped bool   `json:"skipped"`
}

// LadderResult is all five rungs, in the order they ran.
type LadderResult struct {
	Rungs []LadderRung `json:"rungs"`
}

// FirstFailure is the lowest rung that failed -- where to start looking. The second
// return is false when nothing failed.
func (r LadderResult) FirstFailure() (LadderRung, bool) {
	for _, rung := range r.Rungs {
		if !rung.OK && !rung.Skipped {
			return rung, true
		}
	}
	return LadderRung{}, false
}

// OK reports whether every rung that ran passed.
func (r LadderResult) OK() bool {
	for _, rung := range r.Rungs {
		if !rung.Skipped && !rung.OK {
			return false
		}
	}
	return true
}

// Ladder is a configured run. Every sub-probe is required; the real ones are I/O
// and arrive with the collectors, so there is deliberately no default to fall back
// to and a missing one is a panic rather than a rung that quietly did not happen.
//
// OnRung and ShouldContinue are the two optional fields.
type Ladder struct {
	Probes Probes

	// OnRung fires as each rung finishes, so a caller can render a ladder that is
	// still running instead of a screen that sits blank for twenty seconds.
	OnRung func(LadderRung)
	// ShouldContinue is checked before each rung. Returning false marks the
	// remainder skipped and returns early, so an abandoned ladder stops burning
	// subprocess time on a run nobody will read.
	ShouldContinue func() bool

	Net  func() *Info
	Ping func(target string, count, timeoutS int64) *PingSummary
	DNS  func(name string, timeoutS float64) DNSResult
	Curl func(url string, timeoutS, maxRedirects int64) CurlProbe
}

// Run walks all five rungs bottom-up.
func (l Ladder) Run() LadderResult {
	rungs := []LadderRung{}
	var net *Info

	// Built up front because the cancellation branch needs the labels of the rungs
	// it is about to skip, but each step reads the world when it is called: the
	// gateway step sees the snapshot the link step fetched, and nothing runs until
	// the loop reaches it, so a cancelled ladder never spawns the probes it skipped.
	steps := []struct {
		name string
		text string
		run  func(label string) LadderRung
	}{
		{RungLink, "link state", func(label string) LadderRung {
			net = l.Net()
			return linkRung(label, net)
		}},
		{RungGateway, "default gateway", func(label string) LadderRung {
			return l.gatewayRung(label, net)
		}},
		{
			RungInternet,
			fmt.Sprintf("internet (%s)", l.Probes.Ladder.InternetTarget),
			func(label string) LadderRung {
				return l.pingRung(RungInternet, label, l.Probes.Ladder.InternetTarget)
			},
		},
		{RungDNS, fmt.Sprintf("dns (%s)", l.Probes.Ladder.DNSName), l.dnsRung},
		{RungHTTPS, fmt.Sprintf("https (%s)", l.Probes.Ladder.HTTPSURL), l.httpsRung},
	}

	for i, step := range steps {
		if l.ShouldContinue != nil && !l.ShouldContinue() {
			for _, rest := range steps[i:] {
				rungs = append(rungs, LadderRung{
					Name: rest.name, Label: rest.text, Detail: "not run", Skipped: true,
				})
			}
			break
		}
		rung := step.run(step.text)
		rungs = append(rungs, rung)
		if l.OnRung != nil {
			l.OnRung(rung)
		}
	}

	return LadderResult{Rungs: rungs}
}

func linkRung(label string, net *Info) LadderRung {
	if net == nil {
		return LadderRung{
			Name:   RungLink,
			Label:  label,
			Detail: "no default route — this box has no path off itself",
		}
	}
	detail := fmt.Sprintf("%s %s via %s (%s)",
		net.Iface, orText(net.IPv4, "no address"), orNone(net.Gateway), net.Operstate)
	// Only "down" is a real failure. operstate reads "unknown" both for links that
	// are up and passing traffic but whose driver never reports carrier (wireguard,
	// tun/tap, some virtio) *and* as Operstate's own fallback when the sysfs read
	// fails -- neither proves the link is down. Failing on "unknown" would hand rung
	// 1 the verdict on a box whose other four rungs all pass, which is exactly the
	// false attribution this whole module exists to avoid. Regression-tested by
	// name: v1.6.0 shipped `== "up"` here.
	return LadderRung{Name: RungLink, Label: label, OK: net.Operstate != "down", Detail: detail}
}

func (l Ladder) gatewayRung(label string, net *Info) LadderRung {
	if net == nil || net.Gateway == nil || *net.Gateway == "" {
		// Without a gateway address there is nothing to ping, and reporting a failed
		// ping here would be inventing a result.
		return LadderRung{
			Name: RungGateway, Label: label, Detail: "no gateway to test", Skipped: true,
		}
	}
	return l.pingRung(RungGateway, fmt.Sprintf("%s (%s)", label, *net.Gateway), *net.Gateway)
}

func (l Ladder) pingRung(name, label, target string) LadderRung {
	summary := l.Ping(target, ladderPingCount, ladderPingTimeoutS)
	if summary == nil {
		return LadderRung{Name: name, Label: label, Detail: "ping produced no summary"}
	}
	// OK on partial loss: 2/3 back means this rung is up. Quantifying flakiness is
	// the standalone ping probe's job, not the ladder's.
	//
	// %.6g is Python's `:g`: six significant digits with the trailing zeros dropped,
	// so a clean run reads "0% loss" rather than "0.000000% loss".
	return LadderRung{
		Name:  name,
		Label: label,
		OK:    summary.LossPct < 100.0,
		Detail: fmt.Sprintf("%d/%d back, %.6g%% loss, %.1fms avg",
			summary.Received, summary.Sent, summary.LossPct, summary.RTTAvgMS),
	}
}

func (l Ladder) dnsRung(label string) LadderRung {
	result := l.DNS(l.Probes.Ladder.DNSName, l.Probes.DNS.TimeoutS)
	if !result.OK {
		return LadderRung{
			Name: RungDNS, Label: label, Detail: orText(result.Error, "lookup failed"),
		}
	}
	addresses := "no addresses"
	if len(result.Addresses) > 0 {
		addresses = joinComma(result.Addresses)
	}
	return LadderRung{
		Name:   RungDNS,
		Label:  label,
		OK:     true,
		Detail: fmt.Sprintf("%s in %.0fms", addresses, result.LatencyMS),
	}
}

func (l Ladder) httpsRung(label string) LadderRung {
	result := l.Curl(l.Probes.Ladder.HTTPSURL, l.Probes.HTTP.TimeoutS, l.Probes.HTTP.MaxRedirects)
	if result.Error != nil {
		return LadderRung{Name: RungHTTPS, Label: label, Detail: *result.Error}
	}
	// Any status code passes: this rung asks "did a TLS+HTTP exchange complete", and
	// a 401 from an authenticated health endpoint answers that as well as a 200
	// does. CurlProbe.OK is deliberately narrower (2xx/3xx) because the standalone
	// curl probe grades the *service*; grading the service here would report "https
	// is the lowest thing broken" on a healthy host whose ladder.https_url needs
	// auth. Genuine transport failures are the branch above. Regression-tested by
	// name: v1.6.0 shipped result.OK here.
	return LadderRung{
		Name:   RungHTTPS,
		Label:  label,
		OK:     result.HTTPCode > 0,
		Detail: fmt.Sprintf("HTTP %d in %.0fms", result.HTTPCode, result.TimeTotalS*1000),
	}
}

// orText is Python's `x or fallback` over an optional string: a nil pointer and an
// empty string are the same non-answer.
func orText(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

// orNone is an optional string interpolated into an f-string, which is to say a nil
// one renders as the word None. Only reachable with an Info built without a
// gateway, since a default route is what produces one.
func orNone(s *string) string {
	if s == nil {
		return "None"
	}
	return *s
}

func joinComma(items []string) string {
	out := ""
	for i, item := range items {
		if i > 0 {
			out += ", "
		}
		out += item
	}
	return out
}
