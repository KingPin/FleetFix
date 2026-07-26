package network_test

import (
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// Every expected string below is pasted from v1's own output, measured by driving
// src/fleetfix/modules/network/ladder.run_ladder with these same injected values.
// Nothing here is inferred from reading the Python.

// The stock world: one healthy host, so each case says only what it changes.
func okNet() *network.Info {
	return &network.Info{
		Iface:     "eth0",
		IPv4:      ptr("192.168.1.50"),
		Gateway:   ptr("192.168.1.1"),
		Operstate: "up",
		RxBytes:   1,
		TxBytes:   2,
	}
}

func okPing() *network.PingSummary {
	return &network.PingSummary{
		Target: "t", Sent: 3, Received: 3,
		LossPct: 0.0, RTTMinMS: 1.0, RTTAvgMS: 12.34, RTTMaxMS: 3.0, RTTMdevMS: 0.5,
	}
}

func okDNS() network.DNSResult {
	return network.DNSResult{
		Name: "one.one.one.one", OK: true,
		Addresses: []string{"1.1.1.1", "1.0.0.1"}, LatencyMS: 4.6,
	}
}

func okCurl() network.CurlProbe {
	return network.CurlProbe{
		URL: "https://example.invalid", OK: true, HTTPCode: 200,
		TimeTotalS: 0.1234, TimeNamelookupS: 0.01, TimeConnectS: 0.02,
		TimeAppconnectS: 0.03, TimeStarttransferS: 0.04, SizeDownloadBytes: 9,
	}
}

func ptr[T any](v T) *T { return &v }

// world is one scenario's four injected answers.
type world struct {
	net  *network.Info
	ping *network.PingSummary
	dns  network.DNSResult
	curl network.CurlProbe
}

// newWorld starts from the healthy host and applies one deviation, so a nil net or a
// nil ping summary stays sayable -- both are answers a real host gives.
func newWorld(change func(*world)) world {
	w := world{net: okNet(), ping: okPing(), dns: okDNS(), curl: okCurl()}
	if change != nil {
		change(&w)
	}
	return w
}

func (w world) ladder(onRung func(network.LadderRung), shouldContinue func() bool) network.Ladder {
	return network.Ladder{
		Probes:         network.DefaultProbes(),
		OnRung:         onRung,
		ShouldContinue: shouldContinue,
		Net:            func() *network.Info { return w.net },
		Ping:           func(string, int64, int64) *network.PingSummary { return w.ping },
		DNS:            func(string, float64) network.DNSResult { return w.dns },
		Curl:           func(string, int64, int64) network.CurlProbe { return w.curl },
	}
}

// The five rungs of a fully healthy run, reused by every case that changes one.
var (
	passLink = network.LadderRung{
		Name: "link", Label: "link state", OK: true,
		Detail: "eth0 192.168.1.50 via 192.168.1.1 (up)",
	}
	passGateway = network.LadderRung{
		Name: "gateway", Label: "default gateway (192.168.1.1)", OK: true,
		Detail: "3/3 back, 0% loss, 12.3ms avg",
	}
	passInternet = network.LadderRung{
		Name: "internet", Label: "internet (8.8.8.8)", OK: true,
		Detail: "3/3 back, 0% loss, 12.3ms avg",
	}
	passDNS = network.LadderRung{
		Name: "dns", Label: "dns (github.com)", OK: true,
		Detail: "1.1.1.1, 1.0.0.1 in 5ms",
	}
	passHTTPS = network.LadderRung{
		Name: "https", Label: "https (https://github.com)", OK: true,
		Detail: "HTTP 200 in 123ms",
	}
)

// rungs returns the healthy five with the named ones replaced, so a case reads as its
// own deviation rather than five near-identical literals.
func rungs(replace ...network.LadderRung) []network.LadderRung {
	out := []network.LadderRung{passLink, passGateway, passInternet, passDNS, passHTTPS}
	for _, r := range replace {
		for i := range out {
			if out[i].Name == r.Name {
				out[i] = r
			}
		}
	}
	return out
}

func TestLadderRuns(t *testing.T) {
	tests := []struct {
		name   string
		change func(*world)
		want   []network.LadderRung
	}{
		{
			name: "everything passes",
			want: rungs(),
		},
		{
			// Every rung runs anyway: halting here would report "gateway down" on
			// any cloud host whose gateway drops ICMP.
			name:   "a failed rung does not stop the ones above it",
			change: func(w *world) { w.net = withOperstate("down") },
			want: rungs(network.LadderRung{
				Name: "link", Label: "link state", OK: false,
				Detail: "eth0 192.168.1.50 via 192.168.1.1 (down)",
			}),
		},
		{
			name:   "no default route fails the link rung and skips the gateway",
			change: func(w *world) { w.net = nil },
			want: rungs(
				network.LadderRung{
					Name: "link", Label: "link state", OK: false,
					Detail: "no default route — this box has no path off itself",
				},
				skippedGateway,
			),
		},
		{
			name:   "an interface with no address says so",
			change: func(w *world) { w.net = withIPv4(nil) },
			want: rungs(network.LadderRung{
				Name: "link", Label: "link state", OK: true,
				Detail: "eth0 no address via 192.168.1.1 (up)",
			}),
		},
		{
			// The gateway is interpolated without an `or` guard in v1, so a nil one
			// renders as the word None. Faithfully reproduced rather than tidied:
			// this string is what the operator reads.
			name:   "a nil gateway renders as None and skips the gateway rung",
			change: func(w *world) { w.net = withGateway(nil) },
			want: rungs(
				network.LadderRung{
					Name: "link", Label: "link state", OK: true,
					Detail: "eth0 192.168.1.50 via None (up)",
				},
				skippedGateway,
			),
		},
		{
			name:   "an empty gateway is the same non-answer as a nil one",
			change: func(w *world) { w.net = withGateway(ptr("")) },
			want: rungs(
				network.LadderRung{
					Name: "link", Label: "link state", OK: true,
					Detail: "eth0 192.168.1.50 via  (up)",
				},
				skippedGateway,
			),
		},
		{
			name:   "no ping summary fails both ping rungs",
			change: func(w *world) { w.ping = nil },
			want: rungs(
				network.LadderRung{
					Name: "gateway", Label: "default gateway (192.168.1.1)", OK: false,
					Detail: "ping produced no summary",
				},
				network.LadderRung{
					Name: "internet", Label: "internet (8.8.8.8)", OK: false,
					Detail: "ping produced no summary",
				},
			),
		},
		{
			name: "total loss fails a ping rung",
			change: func(w *world) {
				w.ping = &network.PingSummary{
					Target: "t", Sent: 3, Received: 0, LossPct: 100.0,
					RTTMinMS: 1.0, RTTAvgMS: 0.0, RTTMaxMS: 3.0, RTTMdevMS: 0.5,
				}
			},
			want: rungs(
				network.LadderRung{
					Name: "gateway", Label: "default gateway (192.168.1.1)", OK: false,
					Detail: "0/3 back, 100% loss, 0.0ms avg",
				},
				network.LadderRung{
					Name: "internet", Label: "internet (8.8.8.8)", OK: false,
					Detail: "0/3 back, 100% loss, 0.0ms avg",
				},
			),
		},
		{
			// 2/3 back means this rung is up; quantifying the flakiness is the
			// standalone ping probe's job. The percentage is also where Python's `:g`
			// shows up -- six significant digits, trailing zeros dropped.
			name: "partial loss passes and formats the percentage like Python",
			change: func(w *world) {
				w.ping = &network.PingSummary{
					Target: "t", Sent: 3, Received: 2, LossPct: 33.333333333333336,
					RTTMinMS: 1.0, RTTAvgMS: 66.66666, RTTMaxMS: 3.0, RTTMdevMS: 0.5,
				}
			},
			want: rungs(
				network.LadderRung{
					Name: "gateway", Label: "default gateway (192.168.1.1)", OK: true,
					Detail: "2/3 back, 33.3333% loss, 66.7ms avg",
				},
				network.LadderRung{
					Name: "internet", Label: "internet (8.8.8.8)", OK: true,
					Detail: "2/3 back, 33.3333% loss, 66.7ms avg",
				},
			),
		},
		{
			name: "a failed lookup reports the resolver's message",
			change: func(w *world) {
				w.dns = network.DNSResult{
					Name: "one.one.one.one", Addresses: []string{},
					LatencyMS: 4.6, Error: ptr("NXDOMAIN"),
				}
			},
			want: rungs(network.LadderRung{
				Name: "dns", Label: "dns (github.com)", OK: false, Detail: "NXDOMAIN",
			}),
		},
		{
			name: "a failed lookup with no message still says something",
			change: func(w *world) {
				w.dns = network.DNSResult{
					Name: "one.one.one.one", Addresses: []string{}, LatencyMS: 4.6,
				}
			},
			want: rungs(network.LadderRung{
				Name: "dns", Label: "dns (github.com)", OK: false, Detail: "lookup failed",
			}),
		},
		{
			name: "a lookup that succeeded with no addresses passes",
			change: func(w *world) {
				w.dns = network.DNSResult{
					Name: "one.one.one.one", OK: true, Addresses: []string{}, LatencyMS: 0.4,
				}
			},
			want: rungs(network.LadderRung{
				Name: "dns", Label: "dns (github.com)", OK: true,
				Detail: "no addresses in 0ms",
			}),
		},
		{
			name:   "a transport failure fails the https rung",
			change: func(w *world) { w.curl = withCurlError("curl: (60) SSL certificate problem") },
			want: rungs(network.LadderRung{
				Name: "https", Label: "https (https://github.com)", OK: false,
				Detail: "curl: (60) SSL certificate problem",
			}),
		},
		{
			// No error and no status code: nothing to grade, so this rung failed.
			name:   "no status code fails the https rung",
			change: func(w *world) { w.curl = withHTTPCode(0) },
			want: rungs(network.LadderRung{
				Name: "https", Label: "https (https://github.com)", OK: false,
				Detail: "HTTP 0 in 123ms",
			}),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := newWorld(tt.change).ladder(nil, nil).Run()
			if diff := cmp.Diff(tt.want, got.Rungs); diff != "" {
				t.Errorf("rungs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLinkRungPassesOnOperstateUnknown is v1.6.0 regression #1, named so it cannot be
// quietly deleted.
//
// v1.6.0 graded this rung `operstate == "up"`. "unknown" is what the kernel writes
// for a link that is up and passing traffic but whose driver never reports carrier --
// wireguard, tun/tap, some virtio -- and it is also network.Operstate's own fallback
// when the sysfs read fails. Neither proves the link is down, so the bug handed rung
// 1 the verdict on a box whose other four rungs all passed, which is the exact false
// attribution the ladder exists to avoid.
func TestLinkRungPassesOnOperstateUnknown(t *testing.T) {
	for _, state := range []string{"unknown", "dormant", "testing", "lowerlayerdown", ""} {
		t.Run(state, func(t *testing.T) {
			got := newWorld(func(w *world) { w.net = withOperstate(state) }).ladder(nil, nil).Run()

			if !got.Rungs[0].OK {
				t.Errorf(`operstate %q failed the link rung; only "down" is a real failure`, state)
			}
			if !got.OK() {
				t.Errorf("operstate %q made the whole ladder fail on an otherwise healthy host",
					state)
			}
			if _, failed := got.FirstFailure(); failed {
				t.Error("a healthy host reported a first failure")
			}
		})
	}

	// The other half of the rule: "down" must still fail, or the fix for the
	// regression would have taken away the rung's only job.
	down := newWorld(func(w *world) { w.net = withOperstate("down") }).ladder(nil, nil).Run()
	if down.Rungs[0].OK {
		t.Error(`operstate "down" passed the link rung`)
	}
	if first, failed := down.FirstFailure(); !failed || first.Name != network.RungLink {
		t.Errorf("FirstFailure() = %+v, %v; want the link rung", first, failed)
	}
}

// TestHTTPSRungPassesOnAnyStatusCode is v1.6.0 regression #2, named for the same
// reason.
//
// v1.6.0 graded this rung on CurlProbe.OK, which is 2xx/3xx. This rung asks "did a
// TLS+HTTP exchange complete", and a 401 from an authenticated health endpoint
// answers that as well as a 200 does -- so the bug reported "https is the lowest
// thing broken" on a perfectly healthy host whose ladder.https_url needs auth.
// CurlProbe.OK stays narrower on purpose: the standalone curl probe grades the
// *service*, and this rung does not.
func TestHTTPSRungPassesOnAnyStatusCode(t *testing.T) {
	for _, code := range []int64{200, 301, 401, 403, 404, 418, 500, 503} {
		probe := withHTTPCode(code)
		got := newWorld(func(w *world) { w.curl = probe }).ladder(nil, nil).Run()

		if !got.Rungs[4].OK {
			t.Errorf("HTTP %d failed the https rung; the rung grades the exchange, "+
				"not the service", code)
		}
		if !got.OK() {
			t.Errorf("HTTP %d made the whole ladder fail on an otherwise healthy host", code)
		}
	}

	// The other half: a transport failure -- no exchange at all -- must still fail.
	broken := newWorld(func(w *world) {
		w.curl = withCurlError("curl: (7) Failed to connect")
	}).ladder(nil, nil).Run()
	if broken.Rungs[4].OK {
		t.Error("a curl transport error passed the https rung")
	}
}

func TestLadderCancellation(t *testing.T) {
	tests := []struct {
		name  string
		allow int // ShouldContinue returns true this many times, then false
		want  []network.LadderRung
	}{
		{
			name:  "cancelled before the first rung runs nothing",
			allow: 0,
			want: []network.LadderRung{
				notRun("link", "link state"),
				notRun("gateway", "default gateway"),
				notRun("internet", "internet (8.8.8.8)"),
				notRun("dns", "dns (github.com)"),
				notRun("https", "https (https://github.com)"),
			},
		},
		{
			// The remaining rungs are still named, which is why the step list is
			// built up front: a cancelled ladder reports what it did not do.
			name:  "cancelled mid-run keeps what ran and names the rest",
			allow: 2,
			want: []network.LadderRung{
				passLink,
				passGateway,
				notRun("internet", "internet (8.8.8.8)"),
				notRun("dns", "dns (github.com)"),
				notRun("https", "https (https://github.com)"),
			},
		},
		{
			name:  "a ShouldContinue that never cancels changes nothing",
			allow: 99,
			want:  rungs(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			calls := 0
			got := newWorld(nil).ladder(nil, func() bool {
				calls++
				return calls <= tt.allow
			}).Run()
			if diff := cmp.Diff(tt.want, got.Rungs); diff != "" {
				t.Errorf("rungs mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A skipped rung is not a failure -- it was never asked. A ladder cancelled before it
// started therefore reports OK, which is v1's behaviour and is why a caller must read
// the rungs rather than only the verdict.
func TestCancelledLadderIsNotAFailure(t *testing.T) {
	got := newWorld(nil).ladder(nil, func() bool { return false }).Run()

	if !got.OK() {
		t.Error("OK() = false; a ladder of skipped rungs has nothing that failed")
	}
	if first, failed := got.FirstFailure(); failed {
		t.Errorf("FirstFailure() = %+v; a skipped rung is not a failure", first)
	}
}

// Cancelling must stop the probes, not just relabel their results. This is the
// difference between an abandoned ladder that stops burning subprocess time and one
// that finishes a 25-second traceroute nobody will read.
func TestCancellationSkipsTheProbesItNames(t *testing.T) {
	var pings, lookups, curls int
	calls := 0
	l := network.Ladder{
		Probes: network.DefaultProbes(),
		Net:    okNet,
		Ping: func(string, int64, int64) *network.PingSummary {
			pings++
			return okPing()
		},
		DNS: func(string, float64) network.DNSResult {
			lookups++
			return okDNS()
		},
		Curl: func(string, int64, int64) network.CurlProbe {
			curls++
			return okCurl()
		},
		ShouldContinue: func() bool {
			calls++
			return calls <= 2 // link and gateway only
		},
	}

	l.Run()

	if pings != 1 {
		t.Errorf("ping called %d times; want 1 (the gateway rung, not the internet rung)", pings)
	}
	if lookups != 0 || curls != 0 {
		t.Errorf("dns called %d times and curl %d times; want 0 for both", lookups, curls)
	}
}

// OnRung fires per rung as it finishes so a caller can render a ladder that is still
// running. Skipped rungs are not emitted -- v1 builds those outside emit().
func TestOnRungFiresForEachRungThatRan(t *testing.T) {
	tests := []struct {
		name  string
		allow int
		want  []string
	}{
		{"every rung", 99, []string{"link", "gateway", "internet", "dns", "https"}},
		{"cancelled mid-run", 2, []string{"link", "gateway"}},
		{"cancelled before the first", 0, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var seen []string
			calls := 0
			got := newWorld(nil).ladder(
				func(r network.LadderRung) { seen = append(seen, r.Name) },
				func() bool {
					calls++
					return calls <= tt.allow
				},
			).Run()

			if diff := cmp.Diff(tt.want, seen); diff != "" {
				t.Errorf("emitted rungs (-want +got):\n%s", diff)
			}
			// Whatever was emitted must be exactly the rungs that ran, in order, so
			// a caller rendering from OnRung and a caller reading the result agree.
			var ran []string
			for _, r := range got.Rungs {
				if !r.Skipped {
					ran = append(ran, r.Name)
				}
			}
			if diff := cmp.Diff(ran, seen); diff != "" {
				t.Errorf("emitted does not match the rungs that ran (-ran +emitted):\n%s", diff)
			}
		})
	}
}

// The ladder's pings are its own: shorter than the standalone ping probe, because
// this asks "does anything come back", not "how stable is this link". The other two
// probes take their budgets from probes.yml.
func TestLadderPassesTheRightArgumentsToEachProbe(t *testing.T) {
	type pingCall struct {
		Target   string
		Count    int64
		TimeoutS int64
	}
	var pingCalls []pingCall
	var dnsName string
	var dnsTimeout float64
	var curlURL string
	var curlTimeout, curlRedirects int64

	l := network.Ladder{
		Probes: network.DefaultProbes(),
		Net:    okNet,
		Ping: func(target string, count, timeoutS int64) *network.PingSummary {
			pingCalls = append(pingCalls, pingCall{target, count, timeoutS})
			return okPing()
		},
		DNS: func(name string, timeoutS float64) network.DNSResult {
			dnsName, dnsTimeout = name, timeoutS
			return okDNS()
		},
		Curl: func(url string, timeoutS, maxRedirects int64) network.CurlProbe {
			curlURL, curlTimeout, curlRedirects = url, timeoutS, maxRedirects
			return okCurl()
		},
	}
	l.Run()

	wantPings := []pingCall{
		{"192.168.1.1", 3, 6}, // the gateway from Info
		{"8.8.8.8", 3, 6},     // ladder.internet_target
	}
	if diff := cmp.Diff(wantPings, pingCalls); diff != "" {
		t.Errorf("ping calls (-want +got):\n%s", diff)
	}
	if dnsName != "github.com" || dnsTimeout != 3.0 {
		t.Errorf(`dns called with (%q, %v); want ("github.com", 3)`, dnsName, dnsTimeout)
	}
	if curlURL != "https://github.com" || curlTimeout != 15 || curlRedirects != 5 {
		t.Errorf(`curl called with (%q, %v, %v); want ("https://github.com", 15, 5)`,
			curlURL, curlTimeout, curlRedirects)
	}
}

func TestLadderResultVerdicts(t *testing.T) {
	tests := []struct {
		name    string
		rungs   []network.LadderRung
		wantOK  bool
		wantHas bool
		want    string
	}{
		{"empty", nil, true, false, ""},
		{
			"all pass",
			[]network.LadderRung{{Name: "link", OK: true}, {Name: "dns", OK: true}},
			true, false, "",
		},
		{
			// The lowest failure, not the last: it is where to start looking.
			"two failures",
			[]network.LadderRung{
				{Name: "link", OK: true},
				{Name: "internet"},
				{Name: "https"},
			},
			false, true, "internet",
		},
		{
			"a skipped rung is stepped over",
			[]network.LadderRung{
				{Name: "gateway", Skipped: true},
				{Name: "dns", OK: true},
			},
			true, false, "",
		},
		{
			// Skipped wins over OK=false: v1 constructs skipped rungs with ok=False,
			// so reading OK alone would report a failure that never happened.
			"a skipped rung is not a failure even with OK false",
			[]network.LadderRung{{Name: "link", Skipped: true}, {Name: "dns"}},
			false, true, "dns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := network.LadderResult{Rungs: tt.rungs}
			if got := result.OK(); got != tt.wantOK {
				t.Errorf("OK() = %v, want %v", got, tt.wantOK)
			}
			first, has := result.FirstFailure()
			if has != tt.wantHas {
				t.Errorf("FirstFailure() second return = %v, want %v", has, tt.wantHas)
			}
			if first.Name != tt.want {
				t.Errorf("FirstFailure() = %q, want %q", first.Name, tt.want)
			}
		})
	}
}

// --- scenario helpers -------------------------------------------------------

var skippedGateway = network.LadderRung{
	Name: "gateway", Label: "default gateway", Detail: "no gateway to test", Skipped: true,
}

func notRun(name, label string) network.LadderRung {
	return network.LadderRung{Name: name, Label: label, Detail: "not run", Skipped: true}
}

func withOperstate(state string) *network.Info {
	n := okNet()
	n.Operstate = state
	return n
}

func withIPv4(addr *string) *network.Info {
	n := okNet()
	n.IPv4 = addr
	return n
}

func withGateway(addr *string) *network.Info {
	n := okNet()
	n.Gateway = addr
	return n
}

// withHTTPCode keeps OK honest -- 2xx/3xx, as the standalone curl probe grades it --
// precisely so the regression test above proves the https rung ignores it.
func withHTTPCode(code int64) network.CurlProbe {
	c := okCurl()
	c.OK = code >= 200 && code < 400
	c.HTTPCode = code
	return c
}

func withCurlError(msg string) network.CurlProbe {
	c := okCurl()
	c.OK = false
	c.HTTPCode = 0
	c.Error = &msg
	return c
}
