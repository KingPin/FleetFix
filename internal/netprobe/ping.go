package netprobe

import (
	"context"
	"strconv"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// Ping runs ping against a target and reduces it to the numbers a verdict is made
// from, or nil when there is nothing to reduce.
//
// nil covers three different host conditions -- ping is not installed, ping was
// killed by the budget before it printed a summary, and ping printed something
// neither summary pattern matches -- and the collector above turns all three into
// the same "no usable output" finding, as v1 did. It is folded here rather than
// reported separately because the caller's response to each is identical: there is
// no measurement, and the target may or may not be up.
//
// A non-zero exit is not one of those conditions. ping exits 1 on 100% loss having
// printed exactly the summary that says so, and treating that as a failure would
// discard the reading in the one case it matters most.
func (p *Prober) Ping(ctx context.Context, target string, count int64, intervalS float64, timeoutS int64) *network.PingSummary {
	ctx, cancel := withTimeout(ctx, float64(timeoutS))
	defer cancel()

	res, err := p.Run.Run(
		ctx, "ping",
		"-c", strconv.FormatInt(count, 10),
		"-i", pyFloat(intervalS),
		target,
	)
	if err != nil && !cmdrun.IsTimeout(err) {
		// A timeout still carries whatever ping wrote before it was killed, and on
		// a slow link that is often the complete summary. Anything else -- ping
		// missing, or a spawn that failed -- produced no output worth parsing.
		return nil
	}

	// stdout plus stderr, which is v1's `(stdout or "") + (stderr or "")`: ping
	// writes its summary to stdout and "Name or service not known" to stderr, and
	// the parser is given both so an unresolvable target is a failed parse rather
	// than a hang.
	summary, ok := network.ParsePingOutput(target, res.Combined())
	if !ok {
		return nil
	}
	return &summary
}
