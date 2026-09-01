package netprobe

import (
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"syscall"

	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// Port attempts a TCP connect and classifies the outcome.
//
// Resolution happens first and reports dns-error on its own, because layer
// attribution is the whole point of this check: "the name does not resolve" and
// "the port is filtered" are different problems with different fixes, and one
// timeout verdict covering both sends the operator to the wrong team.
//
// The latency clock starts at the connect, not at the lookup, so a slow resolver
// does not read as a slow service. The dns-error paths report the time the lookup
// itself took, which is the number that matters when the lookup is what failed.
func (p *Prober) Port(ctx context.Context, target network.TCPTarget, timeoutS float64) network.PortCheck {
	lookupStart := p.now()
	ctx, cancel := withTimeout(ctx, timeoutS)
	defer cancel()

	addrs, err := p.Lookup.LookupHost(ctx, target.Host)
	if err != nil {
		return network.PortCheck{
			Target:    target,
			State:     network.PortDNSError,
			LatencyMS: p.sinceMS(lookupStart),
			Error:     errText(err.Error()),
		}
	}
	if len(addrs) == 0 {
		// A resolver that answers with no addresses is not an error to Go and is
		// not one to getaddrinfo either, but it leaves nothing to connect to. v1
		// spells the same case out; without it the code would index an empty list.
		return network.PortCheck{
			Target:    target,
			State:     network.PortDNSError,
			LatencyMS: p.sinceMS(lookupStart),
			Error:     errText("name resolved to no addresses"),
		}
	}

	// The first address, as v1 takes infos[0]. Trying every address would be a
	// better connectivity test and a worse diagnostic: the operator wants to know
	// what the host's own resolver order gets them, which is what every other
	// process on the box will get.
	address := net.JoinHostPort(addrs[0], strconv.FormatInt(target.Port, 10))
	connectStart := p.now()
	conn, err := p.Dial.DialContext(ctx, "tcp", address)
	latency := p.sinceMS(connectStart)
	if err == nil {
		_ = conn.Close()
		return network.PortCheck{Target: target, State: network.PortOpen, LatencyMS: latency}
	}

	state, message := classifyDial(err)
	return network.PortCheck{Target: target, State: state, LatencyMS: latency, Error: message}
}

// classifyDial turns a dial failure into one of the four non-open states.
//
// The message is nil for the three states that already say everything: "refused",
// "timeout" and "unreachable" are complete diagnoses, and appending the errno text
// to them would add a syscall name to a line an operator reads. Only the catch-all
// error state carries prose, because there the message is the only content.
func classifyDial(err error) (network.PortState, *string) {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		// Something is alive at that address and nothing is bound to the port. The
		// most informative failure there is: the network path works.
		return network.PortRefused, nil
	case os.IsTimeout(err), errors.Is(err, context.DeadlineExceeded), errors.Is(err, syscall.ETIMEDOUT):
		// Three spellings of "nothing came back". The first is our own deadline
		// expiring mid-dial, the second the context reporting it after the fact,
		// the third the kernel's own SYN timeout -- reachable when the configured
		// budget is generous enough to let the kernel give up first.
		return network.PortTimeout, nil
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return network.PortUnreachable, nil
	default:
		return network.PortError, errText(err.Error())
	}
}
