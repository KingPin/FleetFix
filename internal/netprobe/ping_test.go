package netprobe

import (
	"context"
	"errors"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

const pingClean = `PING 8.8.8.8 (8.8.8.8) 56(84) bytes of data.

--- 8.8.8.8 ping statistics ---
3 packets transmitted, 3 received, 0% packet loss, time 402ms
rtt min/avg/max/mdev = 10.100/12.300/15.700/2.100 ms
`

// 100% loss with the summary intact, and exit 1 to go with it. This is the reading
// the check most needs and the one a naive "non-zero means failure" would discard.
const pingAllLost = `PING 10.1.1.1 (10.1.1.1) 56(84) bytes of data.

--- 10.1.1.1 ping statistics ---
3 packets transmitted, 0 received, 100% packet loss, time 2043ms
`

func TestPingBuildsTheArgvV1Built(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Stdout(pingClean, "ping", "-c", "3", "-i", "0.2", "8.8.8.8")

	summary := p.Ping(context.Background(), "8.8.8.8", 3, 0.2, 6)
	if summary == nil {
		t.Fatal("no summary from clean ping output")
	}
	if summary.Received != 3 || summary.Sent != 3 || summary.LossPct != 0 {
		t.Errorf("summary = %d/%d at %v%% loss", summary.Received, summary.Sent, summary.LossPct)
	}
	if summary.RTTAvgMS != 12.3 {
		t.Errorf("avg rtt = %v", summary.RTTAvgMS)
	}
	if summary.JitterMS() != 2.1 {
		t.Errorf("jitter = %v", summary.JitterMS())
	}
}

// A whole-number interval must reach the argv as "1.0", the way Python's str()
// renders it -- the argv is what a support call compares against a hand-run
// command, and a silently different one wastes the comparison.
func TestPingRendersAWholeNumberIntervalTheWayPythonDoes(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Stdout(pingClean, "ping", "-c", "10", "-i", "1.0", "example.internal")

	if p.Ping(context.Background(), "example.internal", 10, 1.0, 15) == nil {
		t.Fatal("the argv did not match; check the interval rendering")
	}
}

// ping exits 1 on total loss. The exit code is data, and the summary it printed on
// the way out is the entire finding.
func TestPingKeepsTheSummaryFromANonZeroExit(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Exit(1, pingAllLost, "", "ping", "-c", "3", "-i", "0.2", "10.1.1.1")

	summary := p.Ping(context.Background(), "10.1.1.1", 3, 0.2, 6)
	if summary == nil {
		t.Fatal("a 100% loss summary was discarded because ping exited 1")
	}
	if summary.LossPct != 100 {
		t.Errorf("loss = %v%%, want 100", summary.LossPct)
	}
}

// ping writes "Name or service not known" to stderr and nothing to stdout, so a
// probe reading only stdout would hang the parse on an empty string.
func TestPingReadsStderrTooAndReportsNoSummary(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Exit(2, "", "ping: nope.invalid: Name or service not known", "ping", "-c", "3", "-i", "0.2", "nope.invalid")

	if summary := p.Ping(context.Background(), "nope.invalid", 3, 0.2, 6); summary != nil {
		t.Fatalf("got a summary from a failed lookup: %+v", summary)
	}
}

func TestPingIsNilWhenTheBinaryIsAbsent(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Missing("ping", "-c", "3", "-i", "0.2", "8.8.8.8")

	if summary := p.Ping(context.Background(), "8.8.8.8", 3, 0.2, 6); summary != nil {
		t.Fatalf("got a summary with no ping installed: %+v", summary)
	}
}

// A ping killed at its budget still carries whatever it wrote first. On a slow link
// that is often the complete summary, and discarding it would report "no usable
// output" about a measurement we are holding.
func TestPingParsesWhatSurvivedTheBudget(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Partial(
		cmdrun.Result{Stdout: pingClean},
		context.DeadlineExceeded,
		"ping", "-c", "3", "-i", "0.2", "8.8.8.8",
	)

	summary := p.Ping(context.Background(), "8.8.8.8", 3, 0.2, 6)
	if summary == nil {
		t.Fatal("a complete summary was discarded because the budget expired")
	}
	if summary.Received != 3 {
		t.Errorf("received = %d", summary.Received)
	}
}

func TestPingIsNilWhenTheSpawnItselfFailed(t *testing.T) {
	t.Parallel()
	p, fake := newTestProber(t)
	fake.Fail(errors.New("fork/exec: permission denied"), "ping", "-c", "3", "-i", "0.2", "8.8.8.8")

	if summary := p.Ping(context.Background(), "8.8.8.8", 3, 0.2, 6); summary != nil {
		t.Fatalf("got a summary from a failed spawn: %+v", summary)
	}
}
