package netprobe

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

var githubHTTPS = network.TCPTarget{Host: "github.com", Port: 443}

func proberForPort(t *testing.T, addrs []string, dialErr error) (*Prober, *stubDialer) {
	t.Helper()
	p, _ := newTestProber(t)
	p.Now = tickClock(4 * time.Millisecond)
	p.Lookup = &stubResolver{hosts: map[string][]string{"github.com": addrs}}
	dialer := &stubDialer{conn: &stubConn{}, err: dialErr}
	p.Dial = dialer
	return p, dialer
}

func TestPortReportsOpenAndClosesTheSocket(t *testing.T) {
	t.Parallel()
	p, dialer := proberForPort(t, []string{"140.82.121.4"}, nil)
	conn := dialer.conn.(*stubConn)

	check := p.Port(context.Background(), githubHTTPS, 3)
	if check.State != network.PortOpen {
		t.Fatalf("state = %q", check.State)
	}
	if !check.OK() {
		t.Error("an open port is not OK()")
	}
	if check.Error != nil {
		t.Errorf("error = %q on an open port", *check.Error)
	}
	// The resolved address with the target's port, not the hostname: connecting to
	// the name again would resolve twice and could reach a different address.
	if len(dialer.calls) != 1 || dialer.calls[0] != "tcp 140.82.121.4:443" {
		t.Errorf("dialed %v", dialer.calls)
	}
	if !conn.closed {
		t.Error("the probe leaked a connection")
	}
}

// Layer attribution is the whole point: a name that does not resolve must not come
// back as a timeout, which would send the operator to the firewall team.
func TestPortReportsDNSErrorBeforeItDials(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	p.Lookup = &stubResolver{err: errors.New("no such host")}
	dialer := &stubDialer{conn: &stubConn{}}
	p.Dial = dialer

	check := p.Port(context.Background(), githubHTTPS, 3)
	if check.State != network.PortDNSError {
		t.Fatalf("state = %q", check.State)
	}
	if check.Error == nil || *check.Error != "no such host" {
		t.Errorf("error = %v", check.Error)
	}
	if len(dialer.calls) != 0 {
		t.Errorf("dialed %v after the lookup failed", dialer.calls)
	}
}

// A resolver that answers with no addresses is an error to nobody and leaves
// nothing to connect to. Without its own branch this would index an empty slice.
func TestPortReportsDNSErrorForAnEmptyAnswer(t *testing.T) {
	t.Parallel()
	p, dialer := proberForPort(t, nil, nil)

	check := p.Port(context.Background(), githubHTTPS, 3)
	if check.State != network.PortDNSError {
		t.Fatalf("state = %q", check.State)
	}
	if check.Error == nil || *check.Error != "name resolved to no addresses" {
		t.Errorf("error = %v", check.Error)
	}
	if len(dialer.calls) != 0 {
		t.Errorf("dialed %v with nothing to dial", dialer.calls)
	}
}

func TestPortClassifiesDialFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		err   error
		want  network.PortState
		quiet bool // the state says everything; no prose to add
	}{
		{"refused", syscall.ECONNREFUSED, network.PortRefused, true},
		{"kernel SYN timeout", syscall.ETIMEDOUT, network.PortTimeout, true},
		{"our own deadline", context.DeadlineExceeded, network.PortTimeout, true},
		{"host unreachable", syscall.EHOSTUNREACH, network.PortUnreachable, true},
		{"network unreachable", syscall.ENETUNREACH, network.PortUnreachable, true},
		{"anything else", errors.New("protocol not supported"), network.PortError, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// Wrapped in an OpError, which is how the net package actually delivers
			// these -- a classifier matching on the bare errno would pass its unit
			// test and misclassify every real dial.
			wrapped := &net.OpError{Op: "dial", Net: "tcp", Err: tc.err}
			p, _ := proberForPort(t, []string{"140.82.121.4"}, wrapped)

			check := p.Port(context.Background(), githubHTTPS, 3)
			if check.State != tc.want {
				t.Fatalf("state = %q, want %q", check.State, tc.want)
			}
			if tc.quiet && check.Error != nil {
				t.Errorf("error = %q, want none: %q is the whole message", *check.Error, tc.want)
			}
			if !tc.quiet && check.Error == nil {
				t.Error("no message on the catch-all state, which has no other content")
			}
		})
	}
}

// The latency clock starts at the connect. A resolver that takes a second must not
// make a service that answered in 4ms look slow.
func TestPortTimesTheConnectNotTheLookup(t *testing.T) {
	t.Parallel()
	p, _ := proberForPort(t, []string{"140.82.121.4"}, nil)

	check := p.Port(context.Background(), githubHTTPS, 3)
	if check.LatencyMS != 4 {
		t.Fatalf("latency = %vms, want the single tick between connect and answer", check.LatencyMS)
	}
}

// A dns-error reports the lookup's own duration, because there the lookup is what
// failed and its cost is the finding.
func TestPortTimesTheLookupWhenTheLookupIsWhatFailed(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	p.Now = tickClock(250 * time.Millisecond)
	p.Lookup = &stubResolver{err: errors.New("no such host")}

	check := p.Port(context.Background(), githubHTTPS, 3)
	if check.LatencyMS != 250 {
		t.Fatalf("latency = %vms, want the lookup's own duration", check.LatencyMS)
	}
}

func TestPortEchoesTheTargetOnEveryPath(t *testing.T) {
	t.Parallel()
	p, _ := proberForPort(t, []string{"140.82.121.4"}, syscall.ECONNREFUSED)

	check := p.Port(context.Background(), githubHTTPS, 3)
	if check.Target != githubHTTPS {
		t.Fatalf("target = %+v", check.Target)
	}
	if check.OK() {
		t.Error("a refused port is not OK()")
	}
}
