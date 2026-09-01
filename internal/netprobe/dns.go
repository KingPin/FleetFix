package netprobe

import (
	"context"
	"sort"

	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// DNS resolves a name and times the lookup.
//
// Deliberately lower fidelity than `dig +trace`: this answers "does this box
// resolve the names it needs", which is the triage question, and it answers it
// through the same resolver every other process on the host uses. A direct query
// to a nameserver would be measuring something the applications do not do.
func (p *Prober) DNS(ctx context.Context, name string, timeoutS float64) network.DNSResult {
	start := p.now()
	ctx, cancel := withTimeout(ctx, timeoutS)
	defer cancel()

	addrs, err := p.Lookup.LookupHost(ctx, name)
	if err != nil {
		return network.DNSResult{
			Name:      name,
			Addresses: []string{},
			LatencyMS: p.sinceMS(start),
			Error:     errText(err.Error()),
		}
	}
	return network.DNSResult{
		Name:      name,
		OK:        true,
		Addresses: sortedUnique(addrs),
		LatencyMS: p.sinceMS(start),
	}
}

// sortedUnique is v1's `sorted({info[4][0] for info in infos})`.
//
// getaddrinfo returns one entry per socket type, so a name with one address comes
// back three times; the set collapses that. Sorting makes the reading stable, which
// matters because these addresses land in a report two consecutive runs are
// compared byte-for-byte.
func sortedUnique(items []string) []string {
	seen := make(map[string]bool, len(items))
	out := make([]string, 0, len(items))
	for _, item := range items {
		if seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	sort.Strings(out)
	return out
}
