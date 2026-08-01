package network

import (
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// Fixture expectations come from
//
//	python tools/oracle/py_oracle.py --case ping.<name>
//
// and the synthetic ones from feeding the literal to parse_ping_output. The
// target argument matches what the manifest passes for that case.

// Raw is the whole input, so comparing it in every case would bury the numbers
// under a copy of the fixture. One test checks it explicitly instead.
func ignoreRaw() cmp.Option { return cmpopts.IgnoreFields(PingSummary{}, "Raw") }

func TestParsePingOutputFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		target  string
		want    PingSummary
		ok      bool
	}{
		{
			name:    "no loss",
			fixture: "ping/ubuntu_no_loss.txt",
			target:  "8.8.8.8",
			want: PingSummary{
				Target: "8.8.8.8", Sent: 3, Received: 3, LossPct: 0,
				RTTMinMS: 11.913, RTTAvgMS: 12.144, RTTMaxMS: 12.396, RTTMdevMS: 0.198,
			},
			ok: true,
		},
		{
			name:    "partial loss",
			fixture: "ping/partial_loss.txt",
			target:  "flaky.internal",
			want: PingSummary{
				Target: "flaky.internal", Sent: 5, Received: 2, LossPct: 60,
				RTTMinMS: 2.401, RTTAvgMS: 3.118, RTTMaxMS: 3.835, RTTMdevMS: 0.717,
			},
			ok: true,
		},
		{
			// No rtt line at all, so the whole rtt block is zero rather than
			// absent -- the shape the ladder reads together with received == 0.
			name:    "total loss",
			fixture: "ping/total_loss.txt",
			target:  "unreachable",
			want:    PingSummary{Target: "unreachable", Sent: 4, Received: 0, LossPct: 100},
			ok:      true,
		},
		{
			// Debian's "+1 errors" clause sits between the received count and the
			// loss percentage, which is the whole reason that group is optional.
			name:    "errors clause",
			fixture: "ping/debian_plus_errors.txt",
			target:  "router",
			want: PingSummary{
				Target: "router", Sent: 4, Received: 3, LossPct: 25,
				RTTMinMS: 0.901, RTTAvgMS: 1.5, RTTMaxMS: 2.25, RTTMdevMS: 0.58,
			},
			ok: true,
		},
		{
			name:    "unrecognised",
			fixture: "ping/unrecognised.txt",
			target:  "x",
			ok:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParsePingOutput(tt.target, fixture.Text(t, tt.fixture))
			if ok != tt.ok {
				t.Fatalf("ParsePingOutput(%s) ok=%t, want %t", tt.fixture, ok, tt.ok)
			}
			if diff := cmp.Diff(tt.want, got, ignoreRaw()); diff != "" {
				t.Errorf("ParsePingOutput(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

// Raw is the verbatim input, byte for byte -- no trimming, and present even when
// there is no rtt line to report.
func TestParsePingOutputKeepsRawVerbatim(t *testing.T) {
	text := fixture.Text(t, "ping/total_loss.txt")
	got, ok := ParsePingOutput("unreachable", text)
	if !ok {
		t.Fatal("ParsePingOutput(total_loss.txt) found no summary")
	}
	if got.Raw != text {
		t.Errorf("Raw = %q, want the input verbatim", got.Raw)
	}
}

func TestParsePingOutputText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want PingSummary
		ok   bool
	}{
		{
			// The older iputils wording, which is why the group is an
			// alternation rather than an optional "packets ".
			name: "packets received wording",
			in:   "4 packets transmitted, 4 packets received, 0% packet loss\n",
			want: PingSummary{Target: "t", Sent: 4, Received: 4},
			ok:   true,
		},
		{
			// The errors count carries a sign on some builds, either way.
			name: "negative errors count",
			in:   "4 packets transmitted, 3 received, -1 errors, 25% packet loss\n",
			want: PingSummary{Target: "t", Sent: 4, Received: 3, LossPct: 25},
			ok:   true,
		},
		{
			name: "fractional loss",
			in:   "1000 packets transmitted, 999 received, 0.1% packet loss\n",
			want: PingSummary{Target: "t", Sent: 1000, Received: 999, LossPct: 0.1},
			ok:   true,
		},
		{
			// \s* before "ms", so no space is still a match.
			name: "no space before ms",
			in:   "2 packets transmitted, 2 received, 0% packet loss\nrtt min/avg/max/mdev = 1/2/3/4ms\n",
			want: PingSummary{
				Target: "t", Sent: 2, Received: 2,
				RTTMinMS: 1, RTTAvgMS: 2, RTTMaxMS: 3, RTTMdevMS: 4,
			},
			ok: true,
		},
		{
			// \s* spans a newline too. Faithful to v1, and the reason a wrapped
			// rtt line still reads.
			name: "newline before ms",
			in:   "2 packets transmitted, 2 received, 0% packet loss\nrtt min/avg/max/mdev = 1/2/3/4\nms\n",
			want: PingSummary{
				Target: "t", Sent: 2, Received: 2,
				RTTMinMS: 1, RTTAvgMS: 2, RTTMaxMS: 3, RTTMdevMS: 4,
			},
			ok: true,
		},
		{
			// Both patterns search independently, so their order in the output
			// does not matter.
			name: "rtt line above the summary",
			in:   "rtt min/avg/max/mdev = 9/9/9/9 ms\n4 packets transmitted, 4 received, 0% packet loss\n",
			want: PingSummary{
				Target: "t", Sent: 4, Received: 4,
				RTTMinMS: 9, RTTAvgMS: 9, RTTMaxMS: 9, RTTMdevMS: 9,
			},
			ok: true,
		},
		{
			name: "first summary wins",
			in:   "1 packets transmitted, 1 received, 0% packet loss\n2 packets transmitted, 2 received, 50% packet loss\n",
			want: PingSummary{Target: "t", Sent: 1, Received: 1},
			ok:   true,
		},
		{
			// \d is Unicode in Python's re, so this line really does parse there.
			// It is here to hold the pytext classes in place, not because ping
			// emits it.
			name: "non-ascii digits in the counts",
			in:   "٤ packets transmitted, 4 received, 0% packet loss\n",
			want: PingSummary{Target: "t", Sent: 4, Received: 4},
			ok:   true,
		},
		{
			name: "no summary line",
			in:   "nothing useful here\n",
			ok:   false,
		},
		{
			// The loss percentage is required; a truncated summary is not one.
			name: "summary without the loss clause",
			in:   "4 packets transmitted, 4 received\n",
			ok:   false,
		},
		{name: "empty", in: "", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParsePingOutput("t", tt.in)
			if ok != tt.ok {
				t.Fatalf("ParsePingOutput(%q) ok=%t, want %t", tt.in, ok, tt.ok)
			}
			if diff := cmp.Diff(tt.want, got, ignoreRaw()); diff != "" {
				t.Errorf("ParsePingOutput(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// The documented departure, in each of the four shapes that reach it. Python
// raises ValueError out of parse_ping_output for the three malformed numbers and
// returns a summary carrying a big integer for the fourth; here all four report
// no summary, which is what the ladder already knows how to render.
func TestParsePingOutputUnconvertibleNumbersReportNoSummary(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{
			name: "rtt value with two points",
			in:   "4 packets transmitted, 4 received, 0% packet loss\nrtt min/avg/max/mdev = 1.2.3/1/1/1 ms\n",
		},
		{
			name: "loss percentage with two points",
			in:   "4 packets transmitted, 4 received, 0.0.1% packet loss\n",
		},
		{
			name: "loss percentage is a bare point",
			in:   "4 packets transmitted, 4 received, .% packet loss\n",
		},
		{
			// Python reports 99999999999999999999; int64 cannot, and a clamped
			// count would read as a real measurement.
			name: "packet count past int64",
			in:   "99999999999999999999 packets transmitted, 3 received, 0% packet loss\n",
		},
		{
			name: "received count past int64",
			in:   "4 packets transmitted, 99999999999999999999 received, 0% packet loss\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, ok := ParsePingOutput("t", tt.in); ok {
				t.Errorf("ParsePingOutput(%q) = %+v, want no summary", tt.in, got)
			}
		})
	}
}

func TestJitterIsMdev(t *testing.T) {
	got, ok := ParsePingOutput("8.8.8.8", fixture.Text(t, "ping/ubuntu_no_loss.txt"))
	if !ok {
		t.Fatal("ParsePingOutput(ubuntu_no_loss.txt) found no summary")
	}
	if got.JitterMS() != got.RTTMdevMS {
		t.Errorf("JitterMS() = %v, want mdev %v", got.JitterMS(), got.RTTMdevMS)
	}
}

// ping output is the plan's second-highest-entropy shape after traceroute: two
// independent patterns over free text, with an optional clause between two
// required ones.
func FuzzParsePingOutput(f *testing.F) {
	for _, name := range []string{"ubuntu_no_loss", "partial_loss", "total_loss", "debian_plus_errors"} {
		f.Add(fixture.Text(f, "ping/"+name+".txt"))
	}
	f.Add("1 packets transmitted, 1 received, 0% packet loss\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := ParsePingOutput("t", s)
		if !ok {
			if got != (PingSummary{}) {
				t.Fatalf("ParsePingOutput(%q) returned %+v with ok=false", s, got)
			}
			return
		}
		if got.Raw != s {
			t.Fatalf("ParsePingOutput(%q) stored a different Raw: %q", s, got.Raw)
		}
		if got.Sent < 0 || got.Received < 0 {
			t.Fatalf("ParsePingOutput(%q) = sent %d, received %d; neither group can match a sign",
				s, got.Sent, got.Received)
		}
		if got.LossPct < 0 {
			t.Fatalf("ParsePingOutput(%q) = %v%% loss; the group cannot match a sign", s, got.LossPct)
		}
	})
}
