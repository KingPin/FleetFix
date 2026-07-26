package network

import (
	"regexp"
	"slices"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// The two trace tools, recorded on the result: which binary ran is itself
// diagnostic, so it is reported rather than hidden.
const (
	TracerouteTool = "traceroute"
	TracepathTool  = "tracepath"
)

// tracepathNoise are the words tracepath annotates a hop with. They arrive in
// the same position a responder's address would, and are not responders.
var tracepathNoise = map[string]bool{"asymm": true, "pmtu": true, "reached": true}

// Both tools have their own way of saying "this probe got nothing back".
var tracepathNoAnswer = []string{"no reply", "send failed"}

var (
	// "traceroute to 8.8.8.8 (8.8.8.8), 15 hops max, 60 byte packets"
	headerRe = regexp.MustCompile(`traceroute to ` + nonSpace + ` \(([0-9a-fA-F.:]+)\)`)

	// A traceroute hop: leading number, then whatever the probes reported. The
	// rest may be empty -- " 1 " is a hop that reported nothing.
	hopRe = regexp.MustCompile(`^` + optSpaces + `(` + digits + `)` + spaces + `(.*)$`)

	// A tracepath hop: "1:  192.168.1.1  0.687ms reached". The "1?:" form is an
	// MTU discovery probe against [LOCALHOST], not a hop; the '?' between the
	// number and the colon is what keeps it from matching at all.
	tracepathHopRe = regexp.MustCompile(`^` + optSpaces + `(` + digits + `):` + spaces + `(.*)$`)

	// ASCII in v1 and left that way, which has a visible consequence: a tracepath
	// timing written with non-ASCII digits is not found, and the hop keeps its
	// host but loses its rtt. The character class also excludes '_', so
	// "1_0.5ms" matches from the "0" and reads as 0.5.
	tracepathRTTRe = regexp.MustCompile(`([0-9.]+)ms`)

	// "traceroute: unknown host x" / "tracepath: x: Name or service not known".
	// One pattern for both tools, so either prefix is recognised in either
	// tool's output.
	//
	// The `\s*` matches newlines like any other whitespace, so a bare
	// "traceroute:" line takes its message from the next non-empty line. That is
	// v1's behaviour and no output either tool produces reaches it.
	toolErrorRe = regexp.MustCompile(`(?m)^(?:traceroute|tracepath):` + optSpaces + `(.+)$`)
)

// TraceHop is one numbered hop, merged across however many probes reported it.
type TraceHop struct {
	Number int64 `json:"number"`
	// Hosts, RTTsMS and Flags are never nil: an empty hop is an empty list, not
	// a null, on the wire.
	Hosts    []string  `json:"hosts"`
	RTTsMS   []float64 `json:"rtts_ms"`
	Timeouts int       `json:"timeouts"`
	Flags    []string  `json:"flags"`
}

// Responded reports whether anything answered at this hop.
func (h TraceHop) Responded() bool { return len(h.Hosts) > 0 }

// TraceResult is one trace, whichever tool produced it.
//
// Error is set when the tool refused before producing hops. It is a pointer
// because v1 distinguishes "no error" from an error whose message strips to
// nothing -- "traceroute:   " records the empty string, not nil.
type TraceResult struct {
	Target  string     `json:"target"`
	Tool    string     `json:"tool"`
	Hops    []TraceHop `json:"hops"`
	Reached bool       `json:"reached"`
	MaxHops int        `json:"max_hops"`
	Raw     string     `json:"raw"`
	Error   *string    `json:"error"`
}

// LastRespondingHop is the number of the last hop that answered. The second
// return is false when nothing answered.
func (r TraceResult) LastRespondingHop() (int64, bool) {
	for i := len(r.Hops) - 1; i >= 0; i-- {
		if r.Hops[i].Responded() {
			return r.Hops[i].Number, true
		}
	}
	return 0, false
}

// StalledAt is the first hop that never answered, when the trace never reached
// the target. The second return is false when the target was reached, or when
// nothing answered at all -- that is a different diagnosis (no path off this
// box) and deserves its own wording rather than "stalled at hop 1".
func (r TraceResult) StalledAt() (int64, bool) {
	if r.Reached {
		return 0, false
	}
	last, ok := r.LastRespondingHop()
	if !ok {
		return 0, false
	}
	return last + 1, true
}

// ParseTracerouteOutput reads GNU/BSD traceroute output. It always returns a
// result: the hops collected before things went wrong are the diagnostic, so a
// tool error is reported alongside them rather than instead of them.
//
// One departure from v1: a hop whose number will not fit in an int64 is
// dropped, where Python reports the arbitrary-precision integer. Both tools
// print the number from a counter bounded by -m, so this is reachable only by
// output neither tool produces.
func ParseTracerouteOutput(target, output string, maxHops int) TraceResult {
	destination := target
	if m := headerRe.FindStringSubmatch(output); m != nil {
		destination = m[1]
	}

	hops := []TraceHop{}
	for _, line := range pytext.SplitLines(output) {
		m := hopRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		number, err := pytext.Int(m[1])
		if err != nil {
			continue
		}
		hops = append(hops, parseTracerouteHop(number, m[2]))
	}

	out := TraceResult{
		Target:  target,
		Tool:    TracerouteTool,
		Hops:    hops,
		Reached: reachedDestination(hops, destination),
		MaxHops: maxHops,
		Raw:     output,
	}
	if len(hops) == 0 {
		out.Error = toolError(output)
	}
	return out
}

// parseTracerouteHop tokenises one hop's probe results.
//
// Order matters within a hop: an RTT belongs to the host that most recently
// appeared, so "1.1.1.1 1.0 ms 2.2.2.2 2.0 ms" (a load-balanced hop, or -q 3)
// reads as two responders rather than one host with two timings.
func parseTracerouteHop(number int64, rest string) TraceHop {
	hop := TraceHop{Number: number, Hosts: []string{}, RTTsMS: []float64{}, Flags: []string{}}
	for _, word := range pytext.Fields(rest) {
		switch {
		case word == "*":
			hop.Timeouts++
		case word == "ms":
		case strings.HasPrefix(word, "!"):
			hop.Flags = append(hop.Flags, word)
		default:
			// Anything Python's float() reads is a timing, which includes the
			// inf/nan spellings -- a router literally named "inf" would be
			// recorded as a timing rather than a responder. Faithful to v1, and
			// worth knowing before adding a fixture with one in it, since
			// neither value survives a JSON round-trip.
			if v, err := pytext.Float(word); err == nil {
				hop.RTTsMS = append(hop.RTTsMS, v)
			} else {
				hop.Hosts = append(hop.Hosts, word)
			}
		}
	}
	return hop
}

// ParseTracepathOutput reads iputils tracepath output.
//
// tracepath prints one line per *probe*, so a hop appears more than once and
// the lines have to be merged by number, in first-seen order. It also has no
// !H-style flags, and reports completion in a trailer rather than by echoing
// the destination address, so Reached comes from that trailer instead of the
// last hop's host.
//
// Two departures from v1, both on input tracepath does not produce: a hop
// number past int64 drops the line (Python reports the big integer), and a
// probe with nothing after the colon contributes nothing to its hop (Python
// raises IndexError out of the parse, losing every hop already collected).
func ParseTracepathOutput(target, output string, maxHops int) TraceResult {
	merged := map[int64]TraceHop{}
	order := []int64{}

	for _, line := range pytext.SplitLines(output) {
		m := tracepathHopRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		number, err := pytext.Int(m[1])
		if err != nil {
			continue
		}
		hop, seen := merged[number]
		if !seen {
			hop = TraceHop{Number: number, Hosts: []string{}, RTTsMS: []float64{}, Flags: []string{}}
			order = append(order, number)
		}
		merged[number] = mergeTracepathProbe(hop, m[2])
	}

	hops := make([]TraceHop, 0, len(order))
	for _, number := range order {
		hops = append(hops, merged[number])
	}

	out := TraceResult{
		Target: target,
		Tool:   TracepathTool,
		Hops:   hops,
		// Both halves are needed: a maxed-out trace still prints a Resume line.
		Reached: strings.Contains(output, "Resume:") && !strings.Contains(output, "Too many hops"),
		MaxHops: maxHops,
		Raw:     output,
	}
	if len(hops) == 0 {
		out.Error = toolError(output)
	}
	return out
}

func mergeTracepathProbe(hop TraceHop, rest string) TraceHop {
	text := strings.TrimFunc(rest, pytext.IsSpace)
	for _, prefix := range tracepathNoAnswer {
		if strings.HasPrefix(text, prefix) {
			hop.Timeouts++
			return hop
		}
	}

	if fields := pytext.Fields(text); len(fields) > 0 {
		host := fields[0]
		// The same responder answering a second probe is not a second responder.
		if !tracepathNoise[host] && !slices.Contains(hop.Hosts, host) {
			hop.Hosts = append(hop.Hosts, host)
		}
	}

	if m := tracepathRTTRe.FindStringSubmatch(text); m != nil {
		if v, err := pytext.Float(m[1]); err == nil {
			hop.RTTsMS = append(hop.RTTsMS, v)
		}
	}
	return hop
}

func reachedDestination(hops []TraceHop, destination string) bool {
	if len(hops) == 0 {
		return false
	}
	last := hops[len(hops)-1]
	// An ICMP flag on the final hop means some *router* answered for the
	// destination (!X admin-prohibited, !H host-unreachable) -- not the host.
	return len(last.Flags) == 0 && slices.Contains(last.Hosts, destination)
}

func toolError(output string) *string {
	m := toolErrorRe.FindStringSubmatch(output)
	if m == nil {
		return nil
	}
	msg := strings.TrimFunc(m[1], pytext.IsSpace)
	return &msg
}
