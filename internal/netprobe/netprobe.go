// Package netprobe is the I/O half of the network domain.
//
// internal/core/network is deliberately inert: it parses captured text and holds
// the shapes a verdict is made from, and it touches nothing. That is what makes
// the corpus in its testdata/ able to stand in for a network. What it cannot do
// is get the text -- run ping, ask the resolver, open a socket -- and v1 solved
// that by putting the subprocess call in the same function as the parse, which is
// why none of it could be exercised without a host.
//
// Here the two are separate: a Prober is the set of seams that reach the host, and
// every method turns one of them into a shape core/network already defines. The
// collectors in internal/check/builtin/network grade those shapes and never see a
// socket; a test drives the whole domain with a Fake runner, an fstest.MapFS and a
// stub dialer.
//
// Every method answers rather than fails. A missing binary, an unreachable name, a
// /proc that will not read -- each is a fact about the host and comes back as one,
// because the layer above has a status for each and no way to invent one from an
// error it was handed instead of an answer.
package netprobe

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
)

// DefaultResolvConf is where the resolver configuration lives.
//
// Not under hostfs's Proc or Sys roots: those are pseudo-filesystems the kernel
// writes, and /etc/resolv.conf is an ordinary file that a human, DHCP client or
// systemd-resolved wrote. Keeping it a plain path rather than folding a third root
// into hostfs keeps that distinction visible.
const DefaultResolvConf = "/etc/resolv.conf"

// A Dialer opens connections. *net.Dialer satisfies it as written, which is the
// reason for the stdlib shape: the real implementation is not an adapter.
type Dialer interface {
	DialContext(ctx context.Context, network, address string) (net.Conn, error)
}

// A Resolver looks names up. *net.Resolver satisfies it as written.
type Resolver interface {
	LookupHost(ctx context.Context, host string) ([]string, error)
}

// A Prober reaches the host. The zero value is unusable; New builds the live one
// and a test builds one with whichever seams that test needs.
//
// A struct of seams rather than an interface per probe: there are eight of these
// and a test typically stages one, so an interface would make every test implement
// seven methods it does not care about. The fields are exported for the same
// reason -- a test sets what it needs and leaves the rest at the default.
type Prober struct {
	// Run is the subprocess seam: ping, curl, ss, traceroute.
	Run cmdrun.Runner

	// Look answers "is this binary installed", which the trace probe needs before
	// it can decide which of two tools to run.
	Look cmdrun.Looker

	// Host is /proc and /sys, for the default route and the byte counters.
	Host hostfs.Host

	// ResolvConf is the path read for nameservers. A field rather than the
	// constant so a test drives a captured file.
	ResolvConf string

	// ReadFile reads ResolvConf. A seam of its own because resolv.conf sits
	// outside both hostfs roots, and giving it a third root would imply it is a
	// pseudo-filesystem too.
	ReadFile func(name string) (string, error)

	// Dial connects, for the TCP probe and for the source-address trick that
	// finds the primary IPv4.
	Dial Dialer

	// Lookup resolves names, for the DNS probe and for the TCP probe's
	// resolve-before-connect step.
	Lookup Resolver

	// Now is the clock every latency is measured against. Seamed so a test can
	// assert on a number instead of a range.
	Now func() time.Time
}

// New returns a Prober wired to the live host.
func New() *Prober {
	return &Prober{
		Run:        cmdrun.New(),
		Look:       cmdrun.NewPATH(),
		Host:       hostfs.New(),
		ResolvConf: DefaultResolvConf,
		ReadFile:   readOSFile,
		Dial:       &net.Dialer{},
		Lookup:     &net.Resolver{},
		Now:        time.Now,
	}
}

// now is the clock, defaulted, so a Prober built field-by-field in a test does not
// have to set one to avoid a nil call.
func (p *Prober) now() time.Time {
	if p.Now == nil {
		return time.Now()
	}
	return p.Now()
}

// sinceMS is how long something took, in the milliseconds every result reports.
func (p *Prober) sinceMS(start time.Time) float64 {
	return float64(p.now().Sub(start)) / float64(time.Millisecond)
}

// withTimeout applies a probe's own budget on top of whatever the caller's context
// already carries, so the tighter of the two wins.
//
// Seconds arrive as a float because that is what probes.yml holds for three of the
// five. A non-positive budget means "no budget of my own" rather than "expire
// immediately", which is the reading that keeps a misconfigured zero from turning
// every probe into a timeout.
func withTimeout(ctx context.Context, seconds float64) (context.Context, context.CancelFunc) {
	if seconds <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(seconds*float64(time.Second)))
}

// readOSFile is the live ReadFile. os.ReadFile rather than an fs.FS because
// fs.FS paths are relative by contract and resolv.conf is named absolutely; the
// alternative is a third hostfs root, which would imply it is a pseudo-filesystem
// like the other two.
func readOSFile(name string) (string, error) {
	b, err := os.ReadFile(name) //nolint:gosec // the path is a field with a fixed default, not operator input
	return string(b), err
}

// errText is a message pointer for the optional Error fields, which are pointers
// because v1's are `str | None` and an absent message differs from an empty one.
func errText(s string) *string { return &s }

// firstLine is the first non-blank line of a tool's complaint, or the fallback
// when it had none.
//
// One line, because a probe's Error field is a sentence an operator reads in a
// table row: curl's stderr can run to several lines of TLS detail, and the first
// of them is the one that names the problem. The rest survives in Raw.
func firstLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}

// pyFloat renders a float the way Python's str() does, because these values go
// into an argv v1 also built.
//
// The difference that matters is the trailing ".0": Go's shortest form for 1.0 is
// "1", Python's is "1.0". ping accepts both, but the argv is what a test asserts on
// and what a support call compares against a hand-run command, so it matches.
func pyFloat(v float64) string {
	s := strconv.FormatFloat(v, 'g', -1, 64)
	if strings.ContainsAny(s, ".eEni") {
		// A decimal point, an exponent, or one of nan/inf: already unambiguous.
		return s
	}
	return s + ".0"
}
