package network

import (
	"context"
	"net"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// The staged host. Every seam netprobe.Prober takes is a public field, which is
// what lets these tests exercise the collectors over a whole fake host without
// the collector package knowing anything about how a probe is faked.

// A /proc/net/route with one default row: eth0 via 0101A8C0, little-endian for
// 192.168.1.1.
const routeWithDefault = `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
`

const devWithCounters = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets
  eth0: 9000001    4001    0    0    0     0          0         0  7000002    3002
`

const resolvConf = `nameserver 192.168.1.1
nameserver 1.1.1.1
search lan
`

const pingClean = `PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.

--- 8.8.8.8 ping statistics ---
10 packets transmitted, 10 received, 0% packet loss, time 1802ms
rtt min/avg/max/mdev = 10.100/12.300/15.700/2.100 ms
`

const pingLossy = `PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.

--- 8.8.8.8 ping statistics ---
10 packets transmitted, 8 received, 20% packet loss, time 1802ms
rtt min/avg/max/mdev = 10.100/12.300/15.700/2.100 ms
`

const pingDead = `PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.

--- 8.8.8.8 ping statistics ---
10 packets transmitted, 0 received, 100% packet loss, time 9200ms
`

const curl200 = `FLEETFIX_CURL_PROBE
http_code=200
time_namelookup=0.010
time_connect=0.020
time_appconnect=0.060
time_starttransfer=0.100
time_total=0.120
size_download=4096
`

const ssListeners = `LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*    users:(("sshd",pid=812,fd=3))
LISTEN 0      511          0.0.0.0:80         0.0.0.0:*    users:(("nginx",pid=1204,fd=6))
`

const tracerouteReached = `traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets
 1  192.168.1.1  0.687 ms
 2  10.0.0.1  8.114 ms
 3  8.8.8.8  12.443 ms
`

// stubConn answers only what the primary-address probe asks: which address the
// kernel picked, and whether closing works. Embedding net.Conn means anything
// that tried to read from it would panic rather than silently see zero bytes.
type stubConn struct {
	net.Conn
	local net.Addr
}

func (c *stubConn) LocalAddr() net.Addr { return c.local }
func (c *stubConn) Close() error        { return nil }

type stubAddr string

func (stubAddr) Network() string  { return "udp" }
func (a stubAddr) String() string { return string(a) }

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

func (f dialFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

type lookupFunc func(ctx context.Context, host string) ([]string, error)

func (f lookupFunc) LookupHost(ctx context.Context, host string) ([]string, error) {
	return f(ctx, host)
}

// host is a whole staged machine: what its /proc says, what its commands print,
// what is installed, and what its resolver and dialer do.
type host struct {
	prober *netprobe.Prober
	run    *cmdrun.Fake
}

// newHost stages a healthy machine. Each test spoils exactly the one thing it is
// about, so what a test says is what makes it fail.
func newHost(t *testing.T, installed ...string) *host {
	t.Helper()
	if len(installed) == 0 {
		installed = []string{"ping", "curl", "ss", "traceroute"}
	}
	fake := cmdrun.NewFake()
	return &host{
		prober: &netprobe.Prober{
			Run:  fake,
			Look: cmdrun.NewFakeLooker(installed...),
			Host: hostfs.Host{
				Proc: fstest.MapFS{
					"net/route": {Data: []byte(routeWithDefault)},
					"net/dev":   {Data: []byte(devWithCounters)},
				},
				Sys: fstest.MapFS{"class/net/eth0/operstate": {Data: []byte("up\n")}},
			},
			ResolvConf: "/etc/resolv.conf",
			ReadFile:   func(string) (string, error) { return resolvConf, nil },
			Dial: dialFunc(func(context.Context, string, string) (net.Conn, error) {
				return &stubConn{local: stubAddr("10.0.0.5:54321")}, nil
			}),
			Lookup: lookupFunc(func(context.Context, string) ([]string, error) {
				return []string{"140.82.121.4"}, nil
			}),
		},
		run: fake,
	}
}

// probes narrows every list to one target, so a test asserting on a tally is
// counting something it spelled out rather than whatever the defaults ship.
func probes() corenet.Probes {
	p := corenet.DefaultProbes()
	p.Ping.Targets = []string{"8.8.8.8"}
	p.DNS.Names = []string{"github.com"}
	return p
}

// find returns the one check with this id, so a test names the check it means
// rather than indexing into the constructor's order.
func find(t *testing.T, checks []check.Check, id check.ID) check.Check {
	t.Helper()
	for _, c := range checks {
		if c.Spec().ID == id {
			return c
		}
	}
	t.Fatalf("no check %q in the domain", id)
	return nil
}

// run executes a check the way the runner does: a collecting emitter, defaults
// applied, and Normalize on the way out.
func run(t *testing.T, c check.Check, params map[string]string) (check.Result, []check.Event) {
	t.Helper()
	if params == nil {
		params = map[string]string{}
	}
	for _, p := range c.Spec().Params {
		if _, given := params[p.Name]; !given {
			params[p.Name] = p.Default
		}
	}
	var streamed []check.Event
	in := check.Input{
		Params:   params,
		Progress: check.EmitterFunc(func(e check.Event) { streamed = append(streamed, e) }),
	}
	res := c.Run(context.Background(), in)
	// The runner keeps a check's own Steps if it built any and otherwise gives it
	// what it emitted. Mirrored here so res.Steps below is the steps[] the report
	// actually carries, rather than only the half a check chose to build itself.
	if len(res.Steps) == 0 {
		res.Steps = streamed
	}
	return res.Normalize(), streamed
}

// runID stages the whole domain and runs one check out of it, which is how the
// runner reaches them and therefore the only wiring worth testing.
func (h *host) runID(t *testing.T, id check.ID, params map[string]string) (check.Result, []check.Event) {
	t.Helper()
	return run(t, find(t, Checks(h.prober, probes()), id), params)
}

// pingArgv is the argv the ping probe builds for the staged config, spelled once
// so a test staging a response and a test asserting on the tally cannot drift.
func pingArgv(target string) []string {
	return []string{"-c", "10", "-i", "0.2", target}
}

func curlArgv(url string) []string {
	return []string{
		"-sS", "-o", "/dev/null",
		"--max-time", "15",
		"--max-redirs", "5",
		"-L",
		"-w", curlWTemplate(),
		url,
	}
}

// curlWTemplate rebuilds netprobe's -w template from the marker the parser
// looks for. Spelled here rather than exported from netprobe, because a test
// that imported the constant would pass even if the template were wrong.
func curlWTemplate() string {
	return corenet.CurlProbeMarker + "\n" +
		"http_code=%{http_code}\n" +
		"time_namelookup=%{time_namelookup}\n" +
		"time_connect=%{time_connect}\n" +
		"time_appconnect=%{time_appconnect}\n" +
		"time_starttransfer=%{time_starttransfer}\n" +
		"time_total=%{time_total}\n" +
		"size_download=%{size_download}\n"
}

// The domain, as a set. These are the properties a front door depends on and
// that no individual check's test would catch.

func TestEveryCheckInTheDomainIsWellFormed(t *testing.T) {
	t.Parallel()
	seen := map[check.ID]bool{}
	for _, c := range Checks(nil, probes()) {
		spec := c.Spec()
		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", spec.ID, err)
		}
		if seen[spec.ID] {
			t.Errorf("%s is registered twice", spec.ID)
		}
		seen[spec.ID] = true
		// A zero budget means the runner's default, which for a probe that can
		// legitimately sit for half a minute is the wrong number. Every check
		// here computes its own from the configured timeouts.
		if spec.Budget <= 0 {
			t.Errorf("%s has no budget", spec.ID)
		}
	}
	if len(seen) != 9 {
		t.Errorf("the domain ships %d checks", len(seen))
	}
}

// The one check that costs half a minute on a dark path is the one check out of
// the default run. A fleet-wide `fleetfix check` that traced every host would
// take longer than the window most cron jobs get.
func TestOnlyTracerouteIsOutOfTheDefaultRun(t *testing.T) {
	t.Parallel()
	for _, c := range Checks(nil, probes()) {
		spec := c.Spec()
		want := spec.ID != TracerouteID
		if spec.InDefault != want {
			t.Errorf("%s: InDefault = %v", spec.ID, spec.InDefault)
		}
	}
}

// Budgets are computed from probes.yml, so an operator who raises a timeout is
// not cut off by a check budget that did not move with it -- which reads as the
// tool failing rather than the probe taking the time it was told it could.
func TestBudgetsFollowTheConfiguredTimeouts(t *testing.T) {
	t.Parallel()
	slow := probes()
	slow.HTTP.TimeoutS = 120
	slow.Ping.TimeoutS = 90

	base := Checks(nil, probes())
	raised := Checks(nil, slow)
	for _, id := range []check.ID{HTTPSID, PingID, LadderID} {
		was := find(t, base, id).Spec().Budget
		now := find(t, raised, id).Spec().Budget
		if now <= was {
			t.Errorf("%s: budget stayed at %v after its timeout was raised", id, was)
		}
	}
}

// trips[] is empty for the whole domain, deliberately: there is no network rule
// in internal/threshold and inventing one here would smuggle in policy v1 never
// had. The statuses these checks return are their own judgements, which is why
// nothing below passes a threshold.Set in.
func TestTheDomainGradesWithoutThresholds(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(pingClean, "ping", pingArgv("8.8.8.8")...)

	res, _ := h.runID(t, PingID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q with no thresholds supplied", res.Status)
	}
	if len(res.Trips) != 0 {
		t.Errorf("trips = %+v, want none", res.Trips)
	}
}

// Every status a check sets has to be one the report's tally knows, and every
// result has to survive Normalize with non-nil slices.
func TestEveryResultIsAWellFormedDocument(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(pingClean, "ping", pingArgv("8.8.8.8")...)
	h.run.Stdout(curl200, "curl", curlArgv("https://github.com")...)
	h.run.Stdout(ssListeners, "ss", "-tlnpH")
	h.run.Stdout(tracerouteReached, "traceroute", "-n", "-q", "1", "-w", "1", "-N", "16", "-m", "15", "8.8.8.8")

	for _, c := range Checks(h.prober, probes()) {
		id := c.Spec().ID
		res, _ := run(t, c, nil)
		if res.Trips == nil || res.Metrics == nil || res.Steps == nil {
			t.Errorf("%s: a nil slice marshals to null; the wire format wants []", id)
		}
		if res.Summary == "" {
			t.Errorf("%s: no summary, which is the line the TUI and the report both print", id)
		}
		for _, m := range res.Metrics {
			if m.Labels == nil {
				t.Errorf("%s: metric %s has nil labels", id, m.Name)
			}
			if m.Help == "" {
				t.Errorf("%s: metric %s has no HELP line for --prom", id, m.Name)
			}
		}
	}
}

// Text is never markup, and it is never a marker either: an Event carries its
// own Status, and a "✗" baked into the text is drawn twice by anything that
// renders one from the status.
func TestNoStepCarriesItsOwnStatusMarker(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Exit(1, pingDead, "", "ping", pingArgv("8.8.8.8")...)

	_, streamed := h.runID(t, PingID, nil)
	if len(streamed) == 0 {
		t.Fatal("nothing streamed")
	}
	for _, e := range streamed {
		if strings.ContainsAny(e.Text, "✓✗!·⋯") {
			t.Errorf("step text carries a marker: %q", e.Text)
		}
	}
}

func TestVerdictsFoldToTheWorstThingThatHappened(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		statuses []check.Status
		want     check.Status
		summary  string
	}{
		{"all clear", []check.Status{check.StatusOK, check.StatusOK}, check.StatusOK, "all 2 things passed"},
		{"one warning", []check.Status{check.StatusOK, check.StatusWarn}, check.StatusWarn, "2 things ran, 1 with warnings"},
		{
			"a failure outranks a warning",
			[]check.Status{check.StatusWarn, check.StatusCrit},
			check.StatusCrit,
			"1 of 2 things failed",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := verdicts{noun: "things"}
			for _, s := range tc.statuses {
				v.record(check.Discard, s, "detail")
			}
			got := v.result()
			if got.Status != tc.want {
				t.Errorf("status = %q, want %q", got.Status, tc.want)
			}
			if got.Summary != tc.summary {
				t.Errorf("summary = %q, want %q", got.Summary, tc.summary)
			}
		})
	}
}

// An operator who empties a list got what they asked for. Reporting ok would put
// a green row on a check that measured nothing at all.
func TestAnEmptyTargetListIsSkippedRatherThanPassed(t *testing.T) {
	t.Parallel()
	empty := probes()
	empty.Ping.Targets = nil
	empty.DNS.Names = nil
	empty.HTTP.URLs = nil
	empty.TCP.Targets = nil

	h := newHost(t)
	for _, id := range []check.ID{PingID, DNSID, HTTPSID, TCPID} {
		res, _ := run(t, find(t, Checks(h.prober, empty), id), nil)
		if res.Status != check.StatusSkipped {
			t.Errorf("%s: status = %q, want skipped", id, res.Status)
		}
	}
}
