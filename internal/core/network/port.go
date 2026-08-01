package network

// PortState is what a TCP connect attempt became.
//
// A closed enum rather than v1's bare strings, but the same six words on the
// wire. They are the whole point of the check: "the name does not resolve" and
// "the port is filtered" are different problems with different fixes, and a
// single "unreachable" verdict would conflate them into one shrug.
type PortState string

// The six outcomes. dns-error is reported before any connect is attempted, so a
// name that does not resolve never produces a timeout that reads as a firewall.
const (
	PortOpen        PortState = "open"
	PortRefused     PortState = "refused"
	PortTimeout     PortState = "timeout"
	PortUnreachable PortState = "unreachable"
	PortDNSError    PortState = "dns-error"
	PortError       PortState = "error"
)

// PortCheck is one TCP connect attempt: what happened, how long it took, and the
// message when the outcome was not one of the four the state already explains.
//
// The struct without the connect, for the same reason DNSResult is the struct
// without the lookup: connecting is I/O behind a Dialer seam the collectors bring
// with them, and everything that *grades* a connect reads only this shape.
//
// Error is a pointer because v1's is `str | None`, and the distinction survives
// into the report: open, refused, timeout and unreachable each carry nil, since
// the state is the whole message and repeating it as prose would say nothing.
type PortCheck struct {
	Target    TCPTarget `json:"target"`
	State     PortState `json:"state"`
	LatencyMS float64   `json:"latency_ms"`
	Error     *string   `json:"error"`
}

// OK reports whether something was listening.
//
// Only open. refused is a live host with nothing on that port -- useful, and
// distinctly better news than a timeout, but not the thing the operator asked
// about. The collectors grade it as a warning rather than folding it in here.
func (c PortCheck) OK() bool { return c.State == PortOpen }
