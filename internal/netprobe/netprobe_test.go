package netprobe

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/fstest"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
)

// The seams, in the shapes the tests below stage them.

// stubConn is a net.Conn that only answers the two questions the probes ask: what
// address the kernel picked, and whether closing works. Embedding net.Conn rather
// than implementing eight unused methods means a probe that starts reading from a
// connection panics on a nil interface instead of silently getting zero bytes.
type stubConn struct {
	net.Conn
	local  net.Addr
	closed bool
}

func (c *stubConn) LocalAddr() net.Addr { return c.local }

func (c *stubConn) Close() error {
	c.closed = true
	return nil
}

// stubAddr is a net.Addr with a fixed string, so a test can hand back a LocalAddr
// that is deliberately not host:port.
type stubAddr string

func (stubAddr) Network() string  { return "udp" }
func (a stubAddr) String() string { return string(a) }

// stubDialer records what it was asked for and returns whatever the test staged.
type stubDialer struct {
	conn  net.Conn
	err   error
	calls []string
}

func (d *stubDialer) DialContext(_ context.Context, network, address string) (net.Conn, error) {
	d.calls = append(d.calls, network+" "+address)
	if d.err != nil {
		return nil, d.err
	}
	return d.conn, nil
}

// stubResolver answers lookups from a map, so one Prober can serve a test that
// resolves two different names differently.
type stubResolver struct {
	hosts map[string][]string
	err   error
	calls []string
}

func (r *stubResolver) LookupHost(_ context.Context, host string) ([]string, error) {
	r.calls = append(r.calls, host)
	if r.err != nil {
		return nil, r.err
	}
	return r.hosts[host], nil
}

// runnerFunc adapts a function to cmdrun.Runner, for the handful of tests whose
// subject is the context a probe builds rather than the output it parses.
type runnerFunc func(ctx context.Context, name string, args ...string) (cmdrun.Result, error)

func (f runnerFunc) Run(ctx context.Context, name string, args ...string) (cmdrun.Result, error) {
	return f(ctx, name, args...)
}

// resolverFunc adapts a function to Resolver, for tests whose subject is the
// context a probe builds rather than the addresses it gets back.
type resolverFunc func(ctx context.Context, host string) ([]string, error)

func (f resolverFunc) LookupHost(ctx context.Context, host string) ([]string, error) {
	return f(ctx, host)
}

// tickClock advances a fixed step per reading, so every latency in these tests is
// an exact number rather than a range. Two readings apart means one step.
func tickClock(step time.Duration) func() time.Time {
	base := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	var n int64
	return func() time.Time {
		t := base.Add(time.Duration(n) * step)
		n++
		return t
	}
}

func TestPyFloatKeepsThePointZeroPythonWould(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{0.2, "0.2"},
		{1, "1.0"},
		{5, "5.0"},
		{0.05, "0.05"},
		{1.5, "1.5"},
	} {
		if got := pyFloat(tc.in); got != tc.want {
			t.Errorf("pyFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestFirstLineSkipsBlanksAndFallsBack(t *testing.T) {
	t.Parallel()
	if got := firstLine("\n\n  curl: (60) SSL problem\nmore detail\n", "fallback"); got != "curl: (60) SSL problem" {
		t.Errorf("first line = %q", got)
	}
	if got := firstLine("   \n\n", "fallback"); got != "fallback" {
		t.Errorf("empty complaint = %q, want the fallback", got)
	}
}

// A non-positive budget must not mean "expire immediately": a probes.yml that
// somehow yields zero would otherwise turn every probe on the host into a timeout,
// which is the failure mode that looks like a network outage and is not one.
func TestZeroTimeoutLeavesTheContextAlone(t *testing.T) {
	t.Parallel()
	ctx, cancel := withTimeout(context.Background(), 0)
	defer cancel()
	if _, ok := ctx.Deadline(); ok {
		t.Fatal("a zero budget set a deadline")
	}
	if err := ctx.Err(); err != nil {
		t.Fatalf("a zero budget cancelled the context: %v", err)
	}
}

func TestPositiveTimeoutSetsADeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := withTimeout(context.Background(), 30)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("no deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > 30*time.Second {
		t.Fatalf("deadline is %v away, want just under 30s", remaining)
	}
}

// The live Prober is what ships. A nil field here is a probe that panics on a real
// host and on no test, which is the one bug this package cannot afford.
func TestNewWiresEverySeam(t *testing.T) {
	t.Parallel()
	p := New()
	switch {
	case p.Run == nil:
		t.Error("no runner")
	case p.Look == nil:
		t.Error("no looker")
	case p.Host.Proc == nil, p.Host.Sys == nil:
		t.Error("no /proc or /sys")
	case p.ResolvConf != DefaultResolvConf:
		t.Errorf("resolv.conf = %q", p.ResolvConf)
	case p.ReadFile == nil:
		t.Error("no file reader")
	case p.Dial == nil:
		t.Error("no dialer")
	case p.Lookup == nil:
		t.Error("no resolver")
	case p.Now == nil:
		t.Error("no clock")
	}
}

func TestNowAndSinceMSDefaultToTheRealClock(t *testing.T) {
	t.Parallel()
	var p Prober
	start := p.now()
	if start.IsZero() {
		t.Fatal("the default clock returned the zero time")
	}
	if ms := p.sinceMS(start); ms < 0 {
		t.Fatalf("elapsed %v ms, want a non-negative duration", ms)
	}
}

// A staged Prober, so each probe's test says only what it stages.
func newTestProber(t *testing.T) (*Prober, *cmdrun.Fake) {
	t.Helper()
	fake := cmdrun.NewFake()
	return &Prober{
		Run:        fake,
		Look:       cmdrun.NewFakeLooker(),
		Host:       hostfs.Host{Proc: fstest.MapFS{}, Sys: fstest.MapFS{}},
		ResolvConf: "/etc/resolv.conf",
		ReadFile:   func(string) (string, error) { return "", errors.New("no file staged") },
		Dial:       &stubDialer{err: errors.New("no dialer staged")},
		Lookup:     &stubResolver{hosts: map[string][]string{}},
		Now:        tickClock(time.Millisecond),
	}, fake
}
