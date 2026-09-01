package netprobe

import (
	"context"

	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// socketsTimeoutS is v1's `timeout=5` on the ss call, read off the source rather
// than chosen. ss reads a netlink table; five seconds is already generous.
const socketsTimeoutS = 5

// Sockets lists what is listening on TCP.
//
// The error is returned rather than swallowed, which is where this parts company
// with v1: list_listening_sockets returns [] for a missing ss, a non-zero exit and
// a timeout alike, so "nothing is listening" and "we could not ask" are the same
// empty list. On a host with no iproute2 that reads as a box with no services,
// which is exactly the confidently-wrong answer the status enum exists to avoid --
// the collector reports unavailable for a missing ss and error for a broken one.
//
// A non-zero exit with rows on stdout is still an answer. ss exits non-zero when
// it cannot read the process table for some sockets -- routine unprivileged --
// having printed every row it could, and discarding those would hide every
// listener on a non-root run.
func (p *Prober) Sockets(ctx context.Context) ([]network.ListeningSocket, error) {
	ctx, cancel := withTimeout(ctx, socketsTimeoutS)
	defer cancel()

	// -t tcp, -l listening only, -n numeric (rDNS would be slow and, when DNS is
	// the broken thing, wrong), -p the owning process, -H no header row.
	res, err := p.Run.Run(ctx, "ss", "-tlnpH")
	if err != nil {
		return nil, err
	}
	return network.ParseSSOutput(res.Stdout), nil
}
