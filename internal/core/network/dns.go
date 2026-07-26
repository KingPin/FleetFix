package network

// DNSResult is one name lookup: did it resolve, to what, and how long it took.
//
// The struct without the lookup. Resolving is I/O behind a Resolver seam that the
// collectors bring with them, and everything that *grades* a lookup -- the ladder's
// dns rung, the standalone dns check -- reads only this shape. Keeping it here lets
// both be written and tested before the seam exists.
//
// Error is a pointer because v1's is `str | None` and the distinction is real: a
// failed lookup with no message is a different thing from one that never ran.
type DNSResult struct {
	Name      string   `json:"name"`
	OK        bool     `json:"ok"`
	Addresses []string `json:"addresses"`
	LatencyMS float64  `json:"latency_ms"`
	Error     *string  `json:"error"`
}
