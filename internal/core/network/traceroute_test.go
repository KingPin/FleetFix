package network

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Every expectation here was produced by running the v1 parser on the same
// input. The synthetic cases in particular are not reasoned from the regexes --
// several of them are counter-intuitive and were measured.

func hop(number int64, hosts []string, rtts []float64, timeouts int, flags []string) TraceHop {
	h := TraceHop{Number: number, Hosts: []string{}, RTTsMS: []float64{}, Timeouts: timeouts, Flags: []string{}}
	if hosts != nil {
		h.Hosts = hosts
	}
	if rtts != nil {
		h.RTTsMS = rtts
	}
	if flags != nil {
		h.Flags = flags
	}
	return h
}

func str(s string) *string { return &s }

// Raw is the whole fixture; comparing it in every case would bury the fields
// under a wall of text without testing anything the dedicated case below does
// not.
func ignoreRawTrace() cmp.Option { return cmpopts.IgnoreFields(TraceResult{}, "Raw") }

func TestParseTracerouteOutputFixtures(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		target  string
		maxHops int
		want    TraceResult
	}{
		{
			name: "admin prohibited", file: "traceroute/admin_prohibited.txt",
			target: "203.0.113.9", maxHops: 15,
			want: TraceResult{
				Target: "203.0.113.9", Tool: TracerouteTool, MaxHops: 15,
				// The destination answered, but !X means a router answered for
				// it, so the trace did not reach the host.
				Reached: false,
				Hops: []TraceHop{
					hop(1, []string{"192.168.1.1"}, []float64{0.687}, 0, nil),
					hop(2, []string{"10.240.178.229"}, []float64{6.968}, 0, nil),
					hop(3, []string{"203.0.113.9"}, []float64{11.029}, 0, []string{"!X"}),
				},
			},
		},
		{
			name: "multi probe", file: "traceroute/multi_probe.txt",
			target: "1.1.1.1", maxHops: 15,
			want: TraceResult{
				Target: "1.1.1.1", Tool: TracerouteTool, MaxHops: 15, Reached: true,
				Hops: []TraceHop{
					hop(1, []string{"192.168.1.1"}, []float64{0.687, 0.501, 0.442}, 0, nil),
					// A load-balanced hop: two responders on one line, plus a
					// probe that timed out.
					hop(2, []string{"64.15.5.142", "64.15.1.175"}, []float64{11.248, 12.449}, 1, nil),
					hop(3, []string{"1.1.1.1"}, []float64{13.001, 13.1, 13.2}, 0, nil),
				},
			},
		},
		{
			name: "reached", file: "traceroute/reached.txt",
			target: "8.8.8.8", maxHops: 15,
			want: TraceResult{
				Target: "8.8.8.8", Tool: TracerouteTool, MaxHops: 15, Reached: true,
				Hops: []TraceHop{
					hop(1, []string{"192.168.1.1"}, []float64{0.687}, 0, nil),
					hop(2, []string{"10.240.178.229"}, []float64{6.968}, 0, nil),
					hop(3, []string{"8.8.8.8"}, []float64{11.029}, 0, nil),
				},
			},
		},
		{
			name: "stalled", file: "traceroute/stalled.txt",
			target: "192.0.2.1", maxHops: 6,
			want: TraceResult{
				Target: "192.0.2.1", Tool: TracerouteTool, MaxHops: 6,
				Hops: []TraceHop{
					hop(1, []string{"192.168.1.1"}, []float64{0.717}, 0, nil),
					hop(2, []string{"10.240.178.229"}, []float64{12.167}, 0, nil),
					hop(3, []string{"67.59.229.86"}, []float64{9.828}, 0, nil),
					hop(4, nil, nil, 3, nil),
					hop(5, nil, nil, 3, nil),
					hop(6, nil, nil, 3, nil),
				},
			},
		},
		{
			name: "unknown host", file: "traceroute/unknown_host.txt",
			target: "nope.invalid", maxHops: 15,
			want: TraceResult{
				Target: "nope.invalid", Tool: TracerouteTool, MaxHops: 15,
				Hops: []TraceHop{}, Error: str("unknown host nope.invalid"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTracerouteOutput(tt.target, fixture.Text(t, tt.file), tt.maxHops)
			if diff := cmp.Diff(tt.want, got, ignoreRawTrace()); diff != "" {
				t.Errorf("ParseTracerouteOutput(%s) mismatch (-want +got):\n%s", tt.file, diff)
			}
		})
	}
}

func TestParseTracepathOutputFixtures(t *testing.T) {
	tests := []struct {
		name    string
		file    string
		target  string
		maxHops int
		want    TraceResult
	}{
		{
			name: "asymm", file: "tracepath/asymm.txt",
			target: "1.1.1.1", maxHops: 15,
			want: TraceResult{
				Target: "1.1.1.1", Tool: TracepathTool, MaxHops: 15,
				// A Resume line is present, but so is "Too many hops".
				Reached: false,
				Hops: []TraceHop{
					hop(1, []string{"192.168.1.1"}, []float64{0.531}, 0, nil),
					// "asymm  7" trails the timing and contributes neither a
					// host nor a second rtt.
					hop(2, []string{"65.19.100.4"}, []float64{33.087}, 0, nil),
					hop(3, []string{"162.158.61.101"}, []float64{26.459}, 0, nil),
				},
			},
		},
		{
			name: "reached", file: "tracepath/reached.txt",
			target: "192.168.1.1", maxHops: 15,
			want: TraceResult{
				Target: "192.168.1.1", Tool: TracepathTool, MaxHops: 15, Reached: true,
				// Two probes, one responder, both timings.
				Hops: []TraceHop{hop(1, []string{"192.168.1.1"}, []float64{0.789, 0.447}, 0, nil)},
			},
		},
		{
			name: "too many hops", file: "tracepath/too_many_hops.txt",
			target: "192.0.2.1", maxHops: 8,
			want: TraceResult{
				Target: "192.0.2.1", Tool: TracepathTool, MaxHops: 8,
				Hops: []TraceHop{
					hop(1, []string{"192.168.1.1"}, []float64{0.727, 0.418}, 0, nil),
					hop(2, []string{"10.240.178.229"}, []float64{11.988}, 0, nil),
					hop(3, []string{"67.59.229.86"}, []float64{10.429}, 0, nil),
					hop(4, []string{"67.83.221.166"}, []float64{12.94}, 0, nil),
					hop(5, nil, nil, 1, nil),
					hop(6, nil, nil, 1, nil),
					hop(7, nil, nil, 1, nil),
					hop(8, nil, nil, 1, nil),
				},
			},
		},
		{
			name: "unknown host", file: "tracepath/unknown_host.txt",
			target: "nope.invalid", maxHops: 15,
			want: TraceResult{
				Target: "nope.invalid", Tool: TracepathTool, MaxHops: 15,
				Hops: []TraceHop{}, Error: str("nope.invalid: Name or service not known"),
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTracepathOutput(tt.target, fixture.Text(t, tt.file), tt.maxHops)
			if diff := cmp.Diff(tt.want, got, ignoreRawTrace()); diff != "" {
				t.Errorf("ParseTracepathOutput(%s) mismatch (-want +got):\n%s", tt.file, diff)
			}
		})
	}
}

func TestTraceResultKeepsRawVerbatim(t *testing.T) {
	text := fixture.Text(t, "traceroute/stalled.txt")
	if got := ParseTracerouteOutput("192.0.2.1", text, 6).Raw; got != text {
		t.Errorf("Raw = %q, want the output verbatim", got)
	}
	text = fixture.Text(t, "tracepath/asymm.txt")
	if got := ParseTracepathOutput("1.1.1.1", text, 15).Raw; got != text {
		t.Errorf("Raw = %q, want the output verbatim", got)
	}
}

func TestParseTracerouteOutputHopLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []TraceHop
	}{
		{
			// \s+ after the number is required, so a bare number is not a hop.
			name: "number with nothing after it", in: "1\n",
			want: []TraceHop{},
		},
		{
			// ... but the whitespace is enough on its own: (.*) may be empty.
			name: "number and a trailing space", in: "1 \n",
			want: []TraceHop{hop(1, nil, nil, 0, nil)},
		},
		{name: "digits glued to text", in: "12abc\n", want: []TraceHop{}},
		{name: "negative number", in: "-1  1.1.1.1  1.0 ms\n", want: []TraceHop{}},
		{name: "tab separated", in: "1\t1.1.1.1\t1.0 ms\n", want: []TraceHop{hop(1, []string{"1.1.1.1"}, []float64{1}, 0, nil)}},
		{
			// \s is Unicode in Python, and pytext.Space says so.
			name: "leading nbsp", in: "\u00a01  1.1.1.1  1.0 ms\n",
			want: []TraceHop{hop(1, []string{"1.1.1.1"}, []float64{1}, 0, nil)},
		},
		{
			// \d is Unicode too, and int() reads the digits.
			name: "arabic-indic hop number", in: "\u0661  1.1.1.1  1.0 ms\n",
			want: []TraceHop{hop(1, []string{"1.1.1.1"}, []float64{1}, 0, nil)},
		},
		{
			name: "arabic-indic rtt", in: "1  1.1.1.1  \u0661.\u0662 ms\n",
			want: []TraceHop{hop(1, []string{"1.1.1.1"}, []float64{1.2}, 0, nil)},
		},
		{
			// \x1c is a line boundary to splitlines, so the tail becomes its own
			// line -- and "1.0 ms" is not a hop line.
			name: "field separator splits the line", in: "1  1.1.1.1\x1c1.0 ms\n",
			want: []TraceHop{hop(1, []string{"1.1.1.1"}, nil, 0, nil)},
		},
		{name: "stars", in: "1  * * *\n", want: []TraceHop{hop(1, nil, nil, 3, nil)}},
		{name: "bare ms", in: "1  ms\n", want: []TraceHop{hop(1, nil, nil, 0, nil)}},
		{
			// Only the exact lowercase token is dropped.
			name: "uppercase MS is a host", in: "1  1.1.1.1  1.0 MS\n",
			want: []TraceHop{hop(1, []string{"1.1.1.1", "MS"}, []float64{1}, 0, nil)},
		},
		{name: "bare bang is a flag", in: "1  !\n", want: []TraceHop{hop(1, nil, nil, 0, []string{"!"})}},
		{
			// The ! test runs before the float test, so this never reaches it.
			name: "bang with a number after it", in: "1  !1.0\n",
			want: []TraceHop{hop(1, nil, nil, 0, []string{"!1.0"})},
		},
		{
			// Anything float() reads is a timing, and float() reads these.
			name: "underscored number", in: "1  1_0.0  ms\n",
			want: []TraceHop{hop(1, nil, []float64{10}, 0, nil)},
		},
		{
			name: "exponent number", in: "1  1e3 ms\n",
			want: []TraceHop{hop(1, nil, []float64{1000}, 0, nil)},
		},
		{
			// float() does not read hex, so this is a host.
			name: "hex-looking token", in: "1  0x10  ms\n",
			want: []TraceHop{hop(1, []string{"0x10"}, nil, 0, nil)},
		},
		{
			name: "two responders on one line", in: "1  1.1.1.1  1.0 ms 2.2.2.2  2.0 ms\n",
			want: []TraceHop{hop(1, []string{"1.1.1.1", "2.2.2.2"}, []float64{1, 2}, 0, nil)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTracerouteOutput("T", tt.in, 15).Hops
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseTracerouteOutput(%q).Hops mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

func TestParseTracerouteOutputReached(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "header address matches the last hop", in: "traceroute to h (8.8.8.8), 15 hops max\n 1  8.8.8.8  1.0 ms\n", want: true},
		{
			// No header: the destination falls back to the target argument.
			name: "no header, target matches", in: " 1  T  1.0 ms\n", want: true,
		},
		{name: "ipv6 destination", in: "traceroute to h (2001:db8::1)\n 1  2001:db8::1  1.0 ms\n", want: true},
		{name: "uppercase hex destination", in: "traceroute to h (2001:DB8::1)\n 1  2001:DB8::1  1.0 ms\n", want: true},
		{name: "destination among several hosts on the last hop", in: "traceroute to h (8.8.8.8)\n 1  1.1.1.1  1.0 ms 8.8.8.8  2.0 ms\n", want: true},
		{
			// Any flag disqualifies the hop, not just !X and !H.
			name: "last hop carries a flag", in: "traceroute to h (8.8.8.8)\n 1  8.8.8.8  1.0 ms !\n", want: false,
		},
		{name: "destination is not the last hop", in: "traceroute to h (8.8.8.8)\n 1  8.8.8.8  1.0 ms\n 2  * * *\n", want: false},
		{
			// search(), so the first header wins even if a second one follows.
			name: "two headers", in: "traceroute to h (1.1.1.1)\ntraceroute to h (2.2.2.2)\n 1  2.2.2.2  1.0 ms\n", want: false,
		},
		{
			// The address class needs at least one character, so an empty pair
			// of parens leaves the target as the destination.
			name: "empty parens", in: "traceroute to h ()\n 1  8.8.8.8  1.0 ms\n", want: false,
		},
		{name: "non-hex parens", in: "traceroute to h (zz)\n 1  8.8.8.8  1.0 ms\n", want: false},
		{
			// The class stops at 'z', and the closing paren is required, so the
			// header does not match at all.
			name: "trailing junk inside the parens", in: "traceroute to h (8.8.8.8zz)\n 1  8.8.8.8  1.0 ms\n", want: false,
		},
		{
			// search(), not match(), so the header need not start the line.
			name: "header mid-line", in: "x traceroute to h (8.8.8.8) y\n 1  8.8.8.8  1.0 ms\n", want: true,
		},
		{name: "no hops at all", in: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseTracerouteOutput("T", tt.in, 15).Reached; got != tt.want {
				t.Errorf("ParseTracerouteOutput(%q).Reached = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseTracepathOutputProbeLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []TraceHop
	}{
		{
			// The '?' sits between the number and the colon, so an MTU probe
			// never matches the hop pattern.
			name: "mtu discovery probe", in: " 1?: [LOCALHOST]  pmtu 1500\n",
			want: []TraceHop{},
		},
		{name: "no whitespace after the colon", in: "1:x\n", want: []TraceHop{}},
		{name: "no reply", in: "1:  no reply\n", want: []TraceHop{hop(1, nil, nil, 1, nil)}},
		{name: "send failed", in: "1:  send failed\n", want: []TraceHop{hop(1, nil, nil, 1, nil)}},
		{
			// A prefix test, so anything trailing the phrase is still a timeout.
			name: "no reply with a suffix", in: "1:  no reply extra\n",
			want: []TraceHop{hop(1, nil, nil, 1, nil)},
		},
		{
			// ... but it is case-sensitive, so this reads as a responder named
			// "No".
			name: "capitalised No reply", in: "1:  No reply\n",
			want: []TraceHop{hop(1, []string{"No"}, nil, 0, nil)},
		},
		{
			name: "annotation in the host position", in: "1:  asymm 2  0.5ms\n",
			want: []TraceHop{hop(1, nil, []float64{0.5}, 0, nil)},
		},
		{name: "reached alone", in: "1:  reached\n", want: []TraceHop{hop(1, nil, nil, 0, nil)}},
		{
			name: "same responder twice", in: "1:  10.0.0.1  0.5ms\n1:  10.0.0.1  0.7ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1"}, []float64{0.5, 0.7}, 0, nil)},
		},
		{
			name: "two responders under one number", in: "1:  10.0.0.1  0.5ms\n1:  10.0.0.2  0.7ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1", "10.0.0.2"}, []float64{0.5, 0.7}, 0, nil)},
		},
		{
			// First-seen order, not numeric order.
			name: "numbers out of order", in: "2:  10.0.0.2  0.5ms\n1:  10.0.0.1  0.7ms\n",
			want: []TraceHop{
				hop(2, []string{"10.0.0.2"}, []float64{0.5}, 0, nil),
				hop(1, []string{"10.0.0.1"}, []float64{0.7}, 0, nil),
			},
		},
		{
			// search(), so the timing is found wherever it sits in the line.
			name: "timing between annotations", in: "1:  10.0.0.1  asymm 2  0.5ms reached\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1"}, []float64{0.5}, 0, nil)},
		},
		{
			name: "two timings on one probe line", in: "1:  10.0.0.1  0.5ms 0.9ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1"}, []float64{0.5}, 0, nil)},
		},
		{
			// [0-9.]+ matches "1.2.3", which float() then refuses.
			name: "timing with two points", in: "1:  10.0.0.1  1.2.3ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1"}, nil, 0, nil)},
		},
		{name: "timing is a bare point", in: "1:  10.0.0.1  .ms\n", want: []TraceHop{hop(1, []string{"10.0.0.1"}, nil, 0, nil)}},
		{
			// The class is ASCII, so a Unicode-digit timing is simply not found.
			name: "arabic-indic timing", in: "1:  10.0.0.1  \u0660.\u0665ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1"}, nil, 0, nil)},
		},
		{
			// The class excludes '_', so the match starts after it.
			name: "underscored timing", in: "1:  10.0.0.1  1_0.5ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1"}, []float64{0.5}, 0, nil)},
		},
		{
			// "10.0.0.1ms" matches the pattern, and float() refuses the capture.
			name: "ms glued to the host", in: "1:  10.0.0.1ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1ms"}, nil, 0, nil)},
		},
		{name: "hop zero", in: "0:  10.0.0.1  0.5ms\n", want: []TraceHop{hop(0, []string{"10.0.0.1"}, []float64{0.5}, 0, nil)}},
		{
			name: "arabic-indic hop number", in: "\u0661:  10.0.0.1  0.5ms\n",
			want: []TraceHop{hop(1, []string{"10.0.0.1"}, []float64{0.5}, 0, nil)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseTracepathOutput("T", tt.in, 15).Hops
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseTracepathOutput(%q).Hops mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

func TestParseTracepathOutputReached(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want bool
	}{
		{name: "resume line", in: "1:  10.0.0.1  0.5ms reached\nResume: pmtu 1500 hops 1\n", want: true},
		{name: "resume and too many hops", in: "1:  10.0.0.1  0.5ms\nToo many hops: pmtu 1500\nResume: pmtu 1500\n", want: false},
		{
			// A substring test over the whole output, not a per-line one, so a
			// Resume with no hops still counts.
			name: "resume with no hops", in: "Resume: pmtu 1500\n", want: true,
		},
		{name: "lowercase resume", in: "1:  10.0.0.1  0.5ms\nresume: x\n", want: false},
		{name: "too many hops in prose", in: "1:  10.0.0.1  0.5ms\nResume: x\nsome Too many hops text\n", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseTracepathOutput("T", tt.in, 15).Reached; got != tt.want {
				t.Errorf("ParseTracepathOutput(%q).Reached = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestTraceToolError(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want *string
	}{
		{name: "traceroute prefix", in: "traceroute: unknown host nope.invalid\n", want: str("unknown host nope.invalid")},
		{
			// One pattern serves both tools, so either prefix is recognised in
			// either tool's output.
			name: "tracepath prefix", in: "tracepath: bad\n", want: str("bad"),
		},
		{name: "message is stripped", in: "traceroute:  spaced tail  \n", want: str("spaced tail")},
		{name: "no space after the colon", in: "traceroute:nope\n", want: str("nope")},
		{name: "first error line wins", in: "traceroute: first\ntraceroute: second\n", want: str("first")},
		{
			// An empty message is not the absence of one: the field is a pointer
			// so the two stay distinguishable.
			name: "message strips to nothing", in: "traceroute:   \n", want: str(""),
		},
		{
			// \s* matches newlines like any other whitespace.
			name: "message on the following line", in: "traceroute:\nfoo\n", want: str("foo"),
		},
		{name: "nothing follows the colon", in: "traceroute:\n", want: nil},
		{name: "prefix is capitalised", in: "Traceroute: nope\n", want: nil},
		{name: "prefix is not at a line start", in: "x traceroute: nope\n", want: nil},
		{name: "no error at all", in: "", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, got := range []*string{
				ParseTracerouteOutput("T", tt.in, 15).Error,
				ParseTracepathOutput("T", tt.in, 15).Error,
			} {
				if diff := cmp.Diff(tt.want, got); diff != "" {
					t.Errorf("Error for %q mismatch (-want +got):\n%s", tt.in, diff)
				}
			}
		})
	}
}

// An error is only reported when the parse produced nothing: hops collected
// before the tool complained are the diagnostic, and the complaint would
// otherwise read as the whole story.
func TestTraceErrorIsSuppressedWhenHopsExist(t *testing.T) {
	if got := ParseTracerouteOutput("T", "traceroute: bad\n 1  1.1.1.1  1.0 ms\n", 15).Error; got != nil {
		t.Errorf("Error = %q, want nil when hops were parsed", *got)
	}
	if got := ParseTracepathOutput("T", "tracepath: bad\n1:  10.0.0.1  0.5ms\n", 15).Error; got != nil {
		t.Errorf("Error = %q, want nil when hops were parsed", *got)
	}
}

// The two departures from v1, both reachable only on output neither tool
// produces. Named so a future reader finds the reason rather than the symptom.

func TestParseTracerouteOutputHugeHopNumberIsDropped(t *testing.T) {
	in := strings.Repeat("9", 25) + "  1.1.1.1  1.0 ms\n"
	if got := ParseTracerouteOutput("T", in, 15).Hops; len(got) != 0 {
		t.Errorf("Hops = %v, want the hop dropped: Python reports the big integer, int64 cannot", got)
	}
}

func TestParseTracepathOutputHugeHopNumberIsDropped(t *testing.T) {
	in := strings.Repeat("9", 25) + ":  10.0.0.1  0.5ms\n"
	if got := ParseTracepathOutput("T", in, 15).Hops; len(got) != 0 {
		t.Errorf("Hops = %v, want the line dropped: Python reports the big integer, int64 cannot", got)
	}
}

// Python's text.split()[0] raises IndexError here, losing every hop already
// collected. Recording the hop with nothing on it keeps the rest of the trace.
func TestParseTracepathOutputEmptyProbeKeepsTheTrace(t *testing.T) {
	got := ParseTracepathOutput("T", "1:  10.0.0.1  0.5ms\n2:  \n", 15).Hops
	want := []TraceHop{
		hop(1, []string{"10.0.0.1"}, []float64{0.5}, 0, nil),
		hop(2, nil, nil, 0, nil),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Hops mismatch (-want +got):\n%s", diff)
	}
}

func TestTraceResultDerivedHopNumbers(t *testing.T) {
	tests := []struct {
		name       string
		file       string
		target     string
		wantLast   int64
		hasLast    bool
		wantStall  int64
		hasStalled bool
	}{
		{
			name: "stalled after the third hop", file: "traceroute/stalled.txt", target: "192.0.2.1",
			wantLast: 3, hasLast: true, wantStall: 4, hasStalled: true,
		},
		{
			// Reached, so there is nothing to stall at.
			name: "reached", file: "traceroute/reached.txt", target: "8.8.8.8",
			wantLast: 3, hasLast: true,
		},
		{
			// Nothing answered: "no path off this box" is a different diagnosis
			// from "stalled at hop 1".
			name: "no responders", file: "traceroute/unknown_host.txt", target: "nope.invalid",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := ParseTracerouteOutput(tt.target, fixture.Text(t, tt.file), 15)
			last, ok := r.LastRespondingHop()
			if ok != tt.hasLast || (ok && last != tt.wantLast) {
				t.Errorf("LastRespondingHop() = (%v, %v), want (%v, %v)", last, ok, tt.wantLast, tt.hasLast)
			}
			stall, ok := r.StalledAt()
			if ok != tt.hasStalled || (ok && stall != tt.wantStall) {
				t.Errorf("StalledAt() = (%v, %v), want (%v, %v)", stall, ok, tt.wantStall, tt.hasStalled)
			}
		})
	}
}

// A hop with no responder is not a hop that answered, whatever else it carries.
func TestTraceHopResponded(t *testing.T) {
	if hop(1, nil, []float64{1}, 3, []string{"!X"}).Responded() {
		t.Error("a hop with no hosts reported Responded")
	}
	if !hop(1, []string{"1.1.1.1"}, nil, 0, nil).Responded() {
		t.Error("a hop with a host reported not Responded")
	}
}

// The wire form is the differential harness's comparison surface, so the empty
// collections have to survive it as [] rather than null.
func TestTraceResultMarshalsEmptyCollectionsAsLists(t *testing.T) {
	b, err := json.Marshal(ParseTracerouteOutput("T", "1  * * *\n", 15))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"hosts":[]`, `"rtts_ms":[]`, `"flags":[]`, `"error":null`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("marshalled result is missing %s:\n%s", want, b)
		}
	}
	b, err = json.Marshal(ParseTracepathOutput("T", "", 15))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"hops":[]`) {
		t.Errorf("a result with no hops marshalled without an empty list:\n%s", b)
	}
}

func FuzzParseTracerouteOutput(f *testing.F) {
	for _, name := range []string{"reached", "stalled", "multi_probe", "admin_prohibited", "unknown_host"} {
		f.Add(fixture.Text(f, "traceroute/"+name+".txt"))
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := ParseTracerouteOutput("T", s, 15)
		if got.Raw != s {
			t.Fatalf("Raw was altered: %q", got.Raw)
		}
		if got.Tool != TracerouteTool {
			t.Fatalf("Tool = %q", got.Tool)
		}
		if got.Hops == nil {
			t.Fatal("Hops is nil; the wire form must be an empty list")
		}
		if len(got.Hops) > 0 && got.Error != nil {
			t.Fatalf("an error was reported alongside %d hops", len(got.Hops))
		}
		checkHops(t, got)
	})
}

func FuzzParseTracepathOutput(f *testing.F) {
	for _, name := range []string{"reached", "asymm", "too_many_hops", "unknown_host"} {
		f.Add(fixture.Text(f, "tracepath/"+name+".txt"))
	}
	f.Fuzz(func(t *testing.T, s string) {
		got := ParseTracepathOutput("T", s, 15)
		if got.Raw != s {
			t.Fatalf("Raw was altered: %q", got.Raw)
		}
		if got.Tool != TracepathTool {
			t.Fatalf("Tool = %q", got.Tool)
		}
		if got.Hops == nil {
			t.Fatal("Hops is nil; the wire form must be an empty list")
		}
		seen := map[int64]bool{}
		for _, h := range got.Hops {
			if seen[h.Number] {
				t.Fatalf("hop %d appears twice; probes must merge by number", h.Number)
			}
			seen[h.Number] = true
			// tracepath has no !H-style flags, so nothing may ever set one.
			if len(h.Flags) != 0 {
				t.Fatalf("hop %d carries flags %v", h.Number, h.Flags)
			}
			for i, host := range h.Hosts {
				if slices.Contains(h.Hosts[:i], host) {
					t.Fatalf("hop %d lists %q twice", h.Number, host)
				}
			}
		}
		checkHops(t, got)
	})
}

// Invariants both parsers owe regardless of input.
func checkHops(t *testing.T, r TraceResult) {
	t.Helper()
	for _, h := range r.Hops {
		if h.Hosts == nil || h.RTTsMS == nil || h.Flags == nil {
			t.Fatalf("hop %d has a nil collection: %+v", h.Number, h)
		}
		if h.Timeouts < 0 {
			t.Fatalf("hop %d has %d timeouts", h.Number, h.Timeouts)
		}
		for _, host := range h.Hosts {
			if host == "" || strings.ContainsFunc(host, pytext.IsSpace) {
				t.Fatalf("hop %d has host %q; hosts come from split() and cannot be empty or spaced", h.Number, host)
			}
		}
		for _, flag := range h.Flags {
			if !strings.HasPrefix(flag, "!") {
				t.Fatalf("hop %d has flag %q, which does not start with !", h.Number, flag)
			}
		}
	}
	if _, ok := r.StalledAt(); ok && r.Reached {
		t.Fatal("a reached trace reported a stall")
	}
}
