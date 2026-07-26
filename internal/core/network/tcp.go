package network

import (
	"strconv"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// maxPort is the last port a TCP header can name.
const maxPort = 65535

// schemePorts is enough of a scheme table to make a pasted URL or a well-known
// service name work without a port. Not a registry lookup: the point is that an
// operator can paste what their app's config says.
var schemePorts = map[string]int64{
	"https":      443,
	"http":       80,
	"ssh":        22,
	"postgres":   5432,
	"postgresql": 5432,
	"redis":      6379,
	"mysql":      3306,
}

// TCPTarget is a host and port to probe.
type TCPTarget struct {
	Host string `json:"host"`
	Port int64  `json:"port"`
}

// String renders the target the way it would be written in a config, which means
// bracketing an IPv6 literal.
//
// The test for the colon is on the host, not on whether the host is a valid
// address, so a hostname that somehow contains one is bracketed too -- that is
// what makes the rendering re-readable rather than the address family.
//
// It re-reads for every host a DNS lookup or an inet_pton could succeed on, but it
// is not a general round-trip: a host containing a "]" renders to something
// ParseHostPort rejects, because the closing bracket lands early. v1 does the
// same, and nothing that resolves can contain one, so it stays a rendering rather
// than growing an escape.
func (t TCPTarget) String() string {
	port := strconv.FormatInt(t.Port, 10)
	if strings.Contains(t.Host, ":") {
		return "[" + t.Host + "]:" + port
	}
	return t.Host + ":" + port
}

// ParseHostPort reads "host:port", "[::1]:port", or a URL. The second return is
// false when no port can be determined.
//
// A bare hostname never gets a guessed port unless defaultPort supplies one:
// silently probing 80 when the operator meant 5432 produces a confidently wrong
// answer, which is worse than no answer.
//
// defaultPort is a pointer because v1's keyword is `int | None` and the difference
// is load-bearing -- and because v1 never validates it. A default of 0, or -5, or
// 999999 is returned as the target's port unchecked, where the same number written
// into the input would be rejected. Reproduced rather than fixed: the caller that
// passes a bad default is the bug, and a silent clamp here would hide it.
//
// Two things this does not do, both v1's behaviour and both visible in the result:
// userinfo is not stripped, so "user@h:22" reports the host "user@h"; and only a
// "/" ends the authority, so "https://h?q=1" reports the host "h?q=1". A DNS lookup
// on either fails, which is the honest outcome for a string nobody can resolve.
func ParseHostPort(raw string, defaultPort *int64) (TCPTarget, bool) {
	text := strings.TrimFunc(raw, pytext.IsSpace)
	if text == "" {
		return TCPTarget{}, false
	}

	var port *int64

	// URL form: take the scheme's default port, then reduce to the authority. An
	// unknown scheme leaves the port unset rather than failing, so ftp://h still
	// works when the caller supplies a default.
	if scheme, rest, ok := strings.Cut(text, "://"); ok {
		if p, found := schemePorts[pytext.Lower(scheme)]; found {
			port = &p
		}
		authority, _, _ := strings.Cut(rest, "/")
		text = authority
	}

	var host string
	switch {
	case strings.HasPrefix(text, "["):
		// A bracketed IPv6 literal, with or without a port: [::1] or [::1]:5432.
		h, tail, ok := strings.Cut(text[1:], "]")
		if !ok || h == "" {
			return TCPTarget{}, false
		}
		host = h
		if strings.HasPrefix(tail, ":") {
			p, ok := portOrNone(tail[1:])
			if !ok {
				return TCPTarget{}, false
			}
			port = &p
		}
		// Anything else after the bracket -- "[::1]x" -- is neither a port nor an
		// error, so the scheme's port or the default stands. v1 does the same.
	case strings.Count(text, ":") == 1:
		h, portText, _ := strings.Cut(text, ":")
		host = h
		// An explicit port that will not parse is fatal even when the scheme
		// already supplied one, so "https://h:0/x" is no target rather than 443.
		p, ok := portOrNone(portText)
		if !ok {
			return TCPTarget{}, false
		}
		port = &p
	default:
		// Either no colon at all, or a bare IPv6 literal with no brackets, where
		// there is no way to tell the host from the port. Both are the whole string
		// as the host, so "h:443:8" is looked up as that name and fails to resolve.
		host = text
	}

	if host == "" {
		return TCPTarget{}, false
	}
	if port == nil {
		port = defaultPort
	}
	if port == nil {
		return TCPTarget{}, false
	}
	return TCPTarget{Host: host, Port: *port}, true
}

// portOrNone reads a port the way v1 does: Python's int(), then a range check.
//
// So " 443 ", "+443" and "4_43" are all 443, while "0" and "65536" are out of
// range and "443.0" was never a number. A literal too large for an int64 is
// rejected here where v1 rejects it one line later on the range check -- the same
// answer by a different route, so not a divergence.
func portOrNone(text string) (int64, bool) {
	port, err := pytext.Int(text)
	if err != nil {
		return 0, false
	}
	if port < 1 || port > maxPort {
		return 0, false
	}
	return port, true
}
