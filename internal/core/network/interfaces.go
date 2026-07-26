package network

import (
	"io/fs"
	"strconv"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Where the two readers below look under a Host's Proc root.
const (
	ProcNetRoute = "net/route"
	ProcNetDev   = "net/dev"
)

// Counters is one interface's cumulative byte totals.
//
// Cumulative, not a rate: the caller derives throughput from two snapshots and
// the elapsed time between them, which is what keeps this stateless and lets
// the same reading serve a 2-second dashboard tick and a one-shot check.
type Counters struct {
	RxBytes int64 `json:"rx_bytes"`
	TxBytes int64 `json:"tx_bytes"`
}

// DefaultRoute returns the interface and gateway of the default route. ok is
// false when there is no default route, when the file cannot be read, or when
// the first default row's gateway is not a hex word.
//
// That last case stops the search rather than moving on to the next row: v1
// returns from inside the loop, so a malformed gateway means no default route
// at all even when a well-formed row follows it. Preserved deliberately -- a
// /proc/net/route the kernel did not write is not something to paper over.
func DefaultRoute(fsys fs.FS, name string) (iface, gateway string, ok bool) {
	text, err := hostfs.ReadFile(fsys, name)
	if err != nil {
		return "", "", false
	}
	for _, line := range pytext.SplitLines(text) {
		parts := pytext.Fields(line)
		// "Iface" is the header row. Matched by name, so a real interface called
		// Iface would be skipped too -- as it is in v1.
		if len(parts) < 3 || parts[0] == "Iface" {
			continue
		}
		if parts[1] != "00000000" {
			continue
		}
		gw, ok := hexLEToIPv4(parts[2])
		if !ok {
			return "", "", false
		}
		return parts[0], gw, true
	}
	return "", "", false
}

// hexLEToIPv4 renders a little-endian hex word the way /proc/net/route stores
// one: bytes.fromhex, then the octets joined in reverse.
//
// Neither the length nor the range is checked, so this is not "parse an IPv4
// address" -- a two-character word yields "255" and a ten-character one yields
// five components. v1 does the same, and a caller that needs a real address
// should be validating what it got rather than trusting the file.
//
// Odd length or a non-hex character is a ValueError in v1 and !ok here. Working
// in bytes rather than runes is safe for the parity check: any non-ASCII rune
// carries non-hex bytes and so fails anyway, and an all-ASCII string has the
// same length either way. bytes.fromhex also skips ASCII whitespace, which is
// unreachable here because the field came out of a whitespace split.
func hexLEToIPv4(h string) (string, bool) {
	if len(h)%2 != 0 {
		return "", false
	}
	octets := make([]string, len(h)/2)
	for i := 0; i < len(h); i += 2 {
		hi, hiOK := hexDigit(h[i])
		lo, loOK := hexDigit(h[i+1])
		if !hiOK || !loOK {
			return "", false
		}
		octets[len(octets)-1-i/2] = strconv.Itoa(int(hi)<<4 | int(lo))
	}
	return strings.Join(octets, "."), true
}

func hexDigit(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

// ReadCounters parses /proc/net/dev into per-interface byte totals. An
// unreadable file is an empty map, the same non-answer as a file with no rows.
//
// Header rows need no special case: neither carries a colon, and the split is
// what selects data rows.
//
// One departure from v1: a counter past int64 drops that interface's row, where
// Python reports the arbitrary-precision integer. The kernel writes these from
// u64 counters, so the value is not reachable from a real /proc/net/dev.
func ReadCounters(fsys fs.FS, name string) map[string]Counters {
	out := map[string]Counters{}
	text, err := hostfs.ReadFile(fsys, name)
	if err != nil {
		return out
	}
	for _, line := range pytext.SplitLines(text) {
		head, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		// Everything after the first colon must be numeric, so an interface whose
		// name contains one -- an alias such as eth0:1 -- parses as nothing.
		nums := pytext.Fields(rest)
		if len(nums) < 9 {
			continue
		}
		rx, err := pytext.Int(nums[0])
		if err != nil {
			continue
		}
		// Index 8: the first transmit column, immediately after the eight receive
		// ones. Anything past it belongs to columns nothing here reads.
		tx, err := pytext.Int(nums[8])
		if err != nil {
			continue
		}
		out[strings.TrimFunc(head, pytext.IsSpace)] = Counters{RxBytes: rx, TxBytes: tx}
	}
	return out
}
