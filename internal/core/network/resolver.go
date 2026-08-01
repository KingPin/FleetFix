package network

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// SystemdStub is systemd-resolved's local listener. The addresses that actually
// answer queries sit behind it, so seeing only this one is a different
// diagnosis from seeing a real nameserver.
const SystemdStub = "127.0.0.53"

// ResolverConfig is /etc/resolv.conf, reduced to the directives FleetFix reads.
type ResolverConfig struct {
	// Nameservers keeps file order: the resolver asks them in that order, so
	// sorting them would misreport which server gets asked first.
	Nameservers []string `json:"nameservers"`
	Search      []string `json:"search"`
	Options     []string `json:"options"`
	// Source is the path the text came from, for the screen to show. The parser
	// never reads it; the caller passes what it opened.
	Source string `json:"source"`
}

// StubResolver reports whether the stub is the *only* nameserver.
//
// A method rather than a field: it is derived, and the differential harness
// compares stored state, not accessors.
func (c ResolverConfig) StubResolver() bool {
	return len(c.Nameservers) == 1 && c.Nameservers[0] == SystemdStub
}

// ParseResolvConf reads resolv.conf text.
//
// domain and search are mutually exclusive and the last one wins
// (resolv.conf(5)), so both simply overwrite the search list -- including a
// bare "search", which clears it. A bare "domain" does not: it has no argument
// to normalise into a one-element list, so it is ignored and whatever came
// before stands.
func ParseResolvConf(text, source string) ResolverConfig {
	out := ResolverConfig{
		Nameservers: []string{},
		Search:      []string{},
		Options:     []string{},
		Source:      source,
	}

	for _, raw := range pytext.SplitLines(text) {
		// A comment starts at '#' or ';' and may follow a directive on the same
		// line. '#' is cut first, so a ';' inside a '#' comment is already gone.
		line, _, _ := strings.Cut(raw, "#")
		line, _, _ = strings.Cut(line, ";")
		line = strings.TrimFunc(line, pytext.IsSpace)
		if line == "" {
			continue
		}
		parts := pytext.Fields(line)
		keyword, args := parts[0], parts[1:]
		switch {
		case keyword == "nameserver" && len(args) > 0:
			out.Nameservers = append(out.Nameservers, args[0])
		case keyword == "search":
			out.Search = args
		case keyword == "domain" && len(args) > 0:
			// The legacy single-domain form, normalised into a one-element
			// search list. Any further words on the line are dropped.
			out.Search = args[:1]
		case keyword == "options":
			out.Options = append(out.Options, args...)
		}
	}
	return out
}
