package network

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
)

const curl500 = `FLEETFIX_CURL_PROBE
http_code=500
time_namelookup=0.010
time_connect=0.020
time_appconnect=0.060
time_starttransfer=0.100
time_total=0.120
size_download=4096
`

// steppingClock advances a fixed amount on every read, so a latency is a number a
// test can spell out rather than whatever the machine happened to take. Locked
// because the probes it times are what a runner fans out across goroutines.
func steppingClock(step time.Duration) func() time.Time {
	var (
		mu sync.Mutex
		at time.Time
	)
	return func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		at = at.Add(step)
		return at
	}
}

// metricNamed is the one metric with this name, or a failure naming what was there
// instead -- a test that ranged looking for it would pass when nothing was recorded.
func metricNamed(t *testing.T, res check.Result, name string) check.Metric {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no %s metric in %+v", name, res.Metrics)
	return check.Metric{}
}

func TestPingGradesLossInThreeTiers(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		want   check.Status
		step   string
	}{
		{
			"clean", pingClean, check.StatusOK,
			"ping 8.8.8.8  10/10  loss 0%  avg 12.3ms  jitter 2.1ms",
		},
		// Partial loss is a warning and not a pass: an intermittently lossy link is
		// the thing operators chase for weeks, and folding it into ok is how it goes
		// unnoticed until it is total.
		{
			"lossy", pingLossy, check.StatusWarn,
			"ping 8.8.8.8  8/10  loss 20%  avg 12.3ms  jitter 2.1ms",
		},
		{
			"dead", pingDead, check.StatusCrit,
			"ping 8.8.8.8  0/10  loss 100%  avg 0.0ms  jitter 0.0ms",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHost(t)
			// ping exits 1 on total loss having printed the summary that says so,
			// which is why the exit code is not the verdict.
			h.run.Exit(0, tc.output, "", "ping", pingArgv("8.8.8.8")...)

			res, streamed := h.runID(t, PingID, nil)
			if res.Status != tc.want {
				t.Fatalf("status = %q, want %q: %s", res.Status, tc.want, res.Summary)
			}
			if len(streamed) != 1 || streamed[0].Text != tc.step {
				t.Errorf("streamed %+v", streamed)
			}
		})
	}
}

// Loss, latency and jitter are three different faults. An average alone hides the
// link that averages 20ms with 60ms of jitter, which is unusable for anything
// interactive and looks fine on a dashboard graphing means.
func TestPingRecordsLossLatencyAndJitterPerTarget(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(pingClean, "ping", pingArgv("8.8.8.8")...)

	res, _ := h.runID(t, PingID, nil)
	for name, want := range map[string]float64{
		LossPctMetric: 0, RTTAvgMetric: 12.3, JitterMetric: 2.1,
	} {
		m := metricNamed(t, res, name)
		if m.Value != want {
			t.Errorf("%s = %v, want %v", name, m.Value, want)
		}
		if m.Labels["target"] != "8.8.8.8" {
			t.Errorf("%s is not labelled by target: %v", name, m.Labels)
		}
	}
}

// Both causes are named because the operator's next move differs: install
// iputils-ping, or find out why the run did not finish inside its timeout.
func TestPingReportsNoUsableOutputRatherThanZeroLoss(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Missing("ping", pingArgv("8.8.8.8")...)

	res, streamed := h.runID(t, PingID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	want := "ping 8.8.8.8: no usable output (binary missing or timed out)"
	if len(streamed) != 1 || streamed[0].Text != want {
		t.Errorf("streamed %+v", streamed)
	}
	// Nothing was measured, so nothing is recorded. A zero on the loss series
	// would read as a perfect link.
	if len(res.Metrics) != 0 {
		t.Errorf("metrics = %+v, want none from a probe that produced no reading", res.Metrics)
	}
}

// The tally is the check's one status, and the wording is v1's.
func TestPingFoldsEveryTargetIntoOneVerdict(t *testing.T) {
	t.Parallel()
	two := probes()
	two.Ping.Targets = []string{"8.8.8.8", "1.1.1.1"}

	h := newHost(t)
	h.run.Stdout(pingClean, "ping", pingArgv("8.8.8.8")...)
	h.run.Stdout(strings.ReplaceAll(pingLossy, "8.8.8.8", "1.1.1.1"), "ping", pingArgv("1.1.1.1")...)

	res, streamed := run(t, find(t, Checks(h.prober, two), PingID), nil)
	if res.Status != check.StatusWarn {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	if res.Summary != "2 ping targets ran, 1 with warnings" {
		t.Errorf("summary = %q", res.Summary)
	}
	if len(streamed) != 2 {
		t.Errorf("streamed %d steps for 2 targets", len(streamed))
	}
}

func TestDNSReportsWhatEachNameResolvedTo(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.Now = steppingClock(5 * time.Millisecond)

	res, streamed := h.runID(t, DNSID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	if want := "DNS github.com → 140.82.121.4  (5.0ms)"; streamed[0].Text != want {
		t.Errorf("step = %q", streamed[0].Text)
	}
	if m := metricNamed(t, res, DNSLatMetric); m.Value != 5 {
		t.Errorf("latency = %v", m.Value)
	}
}

// A failed lookup still took time, and how long the resolver waited before giving
// up is the difference between a nameserver that refuses and one that is gone.
func TestDNSTimesALookupThatFailed(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.Now = steppingClock(5 * time.Millisecond)
	h.prober.Lookup = lookupFunc(func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	})

	res, streamed := h.runID(t, DNSID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	if want := "DNS github.com: no such host  (5.0ms)"; streamed[0].Text != want {
		t.Errorf("step = %q", streamed[0].Text)
	}
	if m := metricNamed(t, res, DNSLatMetric); m.Value != 5 {
		t.Errorf("latency = %v, want the time the lookup spent before failing", m.Value)
	}
}

// A resolver that answers with no addresses is not an error and is not a working
// lookup either. It has to read as itself rather than as a gap after the arrow.
func TestDNSSaysSoWhenANameResolvedToNothing(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.Lookup = lookupFunc(func(context.Context, string) ([]string, error) {
		return nil, nil
	})

	_, streamed := h.runID(t, DNSID, nil)
	if !strings.Contains(streamed[0].Text, "no addresses") {
		t.Errorf("step = %q", streamed[0].Text)
	}
}

// v1's rule, and the one worth stating: a 5xx means the server answered, which
// means DNS, the route, the connect and the TLS handshake all worked. Grading it
// crit makes an endpoint that returns 401 by design look like an outage.
func TestHTTPSWarnsWhenTheServerAnsweredBadly(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		output string
		want   check.Status
	}{
		{"200", curl200, check.StatusOK},
		{"500", curl500, check.StatusWarn},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHost(t)
			h.run.Stdout(tc.output, "curl", curlArgv("https://github.com")...)

			res, streamed := h.runID(t, HTTPSID, nil)
			if res.Status != tc.want {
				t.Fatalf("status = %q, want %q: %s", res.Status, tc.want, res.Summary)
			}
			// The timing breakdown is the point of shelling out to curl at all:
			// slow DNS, a slow connect and a slow handshake are three different
			// problems and one total time cannot tell them apart.
			if !strings.Contains(streamed[0].Text, "dns 10.0ms · connect 20.0ms · tls 60.0ms · ttfb 100.0ms") {
				t.Errorf("step = %q", streamed[0].Text)
			}
		})
	}
}

// No exchange happened: DNS, the connect or the handshake. curl's own sentence is
// what an operator can act on, and there is nothing to measure.
func TestHTTPSFailsWithCurlsOwnReasonWhenNothingWasExchanged(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Exit(6, "", "curl: (6) Could not resolve host: github.com",
		"curl", curlArgv("https://github.com")...)

	res, streamed := h.runID(t, HTTPSID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	want := "curl https://github.com: curl: (6) Could not resolve host: github.com"
	if streamed[0].Text != want {
		t.Errorf("step = %q", streamed[0].Text)
	}
	// A zero on the timing series would read as an instant response.
	if len(res.Metrics) != 0 {
		t.Errorf("metrics = %+v, want none when no exchange happened", res.Metrics)
	}
}

func TestTCPGradesEachStateByHowMuchItProves(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want check.Status
		line string
	}{
		{"open", nil, check.StatusOK, "tcp github.com:443 → open in 5ms"},
		// Refused is a warning because it is an answer: the route, the firewall and
		// the host all work and only the service is not listening. A timeout could
		// be any of the four.
		{"refused", syscall.ECONNREFUSED, check.StatusWarn, "tcp github.com:443 → refused in 5ms"},
		{"timeout", context.DeadlineExceeded, check.StatusCrit, "tcp github.com:443 → timeout in 5ms"},
		{"unreachable", syscall.EHOSTUNREACH, check.StatusCrit, "tcp github.com:443 → unreachable in 5ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newHost(t)
			h.prober.Now = steppingClock(5 * time.Millisecond)
			if tc.err != nil {
				h.prober.Dial = dialFunc(func(context.Context, string, string) (net.Conn, error) {
					return nil, tc.err
				})
			}

			res, streamed := h.runID(t, TCPID, nil)
			if res.Status != tc.want {
				t.Fatalf("status = %q, want %q: %s", res.Status, tc.want, res.Summary)
			}
			if streamed[0].Text != tc.line {
				t.Errorf("step = %q, want %q", streamed[0].Text, tc.line)
			}
			if m := metricNamed(t, res, TCPConnMetric); m.Labels["target"] != "github.com:443" {
				t.Errorf("metric labels = %v", m.Labels)
			}
		})
	}
}

// Layer attribution is the whole reason the probe resolves before it dials: "the
// name does not resolve" and "the port is filtered" go to different teams.
func TestTCPSeparatesANameThatWillNotResolveFromAPortThatWillNotAnswer(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.Lookup = lookupFunc(func(context.Context, string) ([]string, error) {
		return nil, errors.New("no such host")
	})

	res, streamed := h.runID(t, TCPID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	if !strings.Contains(streamed[0].Text, string(corenet.PortDNSError)) {
		t.Errorf("step = %q, want the dns layer named", streamed[0].Text)
	}
	// The reason is carried for the catch-all states, where the prose is the only
	// content the line has.
	if !strings.Contains(streamed[0].Text, "no such host") {
		t.Errorf("step = %q, want the resolver's own message", streamed[0].Text)
	}
}

// Two of the four shell out and two do not, and which is which decides what a
// minimal container can still answer.
func TestOnlyThePingAndCurlProbesNeedABinary(t *testing.T) {
	t.Parallel()
	all := Checks(nil, probes())
	for id, want := range map[check.ID][]string{
		PingID:  {"ping"},
		HTTPSID: {"curl"},
		DNSID:   nil,
		TCPID:   nil,
	} {
		got := find(t, all, id).Spec().NeedsBins
		if len(got) != len(want) || (len(want) == 1 && got[0] != want[0]) {
			t.Errorf("%s needs %v, want %v", id, got, want)
		}
	}
}
