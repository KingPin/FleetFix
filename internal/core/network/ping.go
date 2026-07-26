// Package network parses the output of the network tools FleetFix shells out to
// -- ping, ss, traceroute, tracepath, curl and the /proc counters behind them.
//
// No I/O: every function here takes captured text and returns a struct, which is
// what makes the corpus in testdata/ able to stand in for a network.
package network

import (
	"regexp"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Regex fragments shared across the package's parsers. Python's \s and \d are
// Unicode where Go's are ASCII, so spelling them out is not decoration -- see
// pytext.Space and pytext.Digit.
const (
	optSpaces = pytext.Space + `*`
	spaces    = pytext.Space + `+`
	nonSpace  = pytext.NotSpace + `+`
	digits    = pytext.Digit + `+`
)

const (
	// The rtt line's own character class, ASCII in v1 and left that way: iputils
	// prints these with printf, so a Unicode digit here would mean something
	// other than ping produced the line.
	asciiNumber = `[0-9.]+`
	// The loss percentage, which v1 does write with \d.
	number = `[\p{Nd}.]+`
)

var (
	// "rtt min/avg/max/mdev = 11.913/12.144/12.396/0.198 ms"
	rttRe = regexp.MustCompile(`rtt min/avg/max/mdev = ` +
		`(` + asciiNumber + `)/(` + asciiNumber + `)/(` + asciiNumber + `)/(` + asciiNumber + `)` +
		optSpaces + `ms`)

	// "4 packets transmitted, 3 received, +1 errors, 25% packet loss, time 3050ms"
	//
	// The errors clause is Debian's and optional; "packets received" is the older
	// iputils wording. Both spellings are in the corpus.
	lossRe = regexp.MustCompile(`(` + digits + `) packets transmitted, ` +
		`(` + digits + `) (?:received|packets received), ` +
		`(?:[+\-]?` + digits + ` errors, )?` +
		`(` + number + `)% packet loss`)
)

// PingSummary is one ping run, reduced to the numbers the ladder grades on.
type PingSummary struct {
	Target    string  `json:"target"`
	Sent      int64   `json:"sent"`
	Received  int64   `json:"received"`
	LossPct   float64 `json:"loss_pct"`
	RTTMinMS  float64 `json:"rtt_min_ms"`
	RTTAvgMS  float64 `json:"rtt_avg_ms"`
	RTTMaxMS  float64 `json:"rtt_max_ms"`
	RTTMdevMS float64 `json:"rtt_mdev_ms"`
	// Raw is the verbatim command output, so a screen can show what ping actually
	// said rather than only this extraction of it.
	Raw string `json:"raw"`
}

// JitterMS is the conventional name for mdev.
//
// A method rather than a field: it is derived, and the differential harness
// compares stored state, not accessors.
func (p PingSummary) JitterMS() float64 { return p.RTTMdevMS }

// ParsePingOutput reads an iputils ping summary. The second return is false when
// the output has no summary in it -- a ping that never got started, or a tool
// whose wording neither pattern covers.
//
// It is also false when a captured number will not convert, which is the one
// place this parts company with v1: Python raises ValueError out of the parse and
// takes the caller down with it, where here the reading is simply unavailable.
// That is the same conclusion the ladder already draws from a ping that produced
// no summary at all, and it cannot be reached by output ping actually emits --
// the numbers come from printf on a counter.
func ParsePingOutput(target, output string) (PingSummary, bool) {
	loss := lossRe.FindStringSubmatch(output)
	if loss == nil {
		return PingSummary{}, false
	}
	sent, err := pytext.Int(loss[1])
	if err != nil {
		return PingSummary{}, false
	}
	received, err := pytext.Int(loss[2])
	if err != nil {
		return PingSummary{}, false
	}
	lossPct, err := pytext.Float(loss[3])
	if err != nil {
		return PingSummary{}, false
	}

	out := PingSummary{
		Target:   target,
		Sent:     sent,
		Received: received,
		LossPct:  lossPct,
		Raw:      output,
	}

	rtt := rttRe.FindStringSubmatch(output)
	if rtt == nil {
		// The total-loss case: no rtt line at all, so the rtt block stays zero.
		return out, true
	}
	dst := []*float64{&out.RTTMinMS, &out.RTTAvgMS, &out.RTTMaxMS, &out.RTTMdevMS}
	for i, p := range dst {
		v, err := pytext.Float(rtt[i+1])
		if err != nil {
			return PingSummary{}, false
		}
		*p = v
	}
	return out, true
}
