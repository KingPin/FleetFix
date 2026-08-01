package network

import (
	"math"
	"math/big"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

func TestDefaultProbes(t *testing.T) {
	got := DefaultProbes()
	want := Probes{
		Ping: PingProbes{
			Targets:   []string{"8.8.8.8", "1.1.1.1"},
			Count:     10,
			IntervalS: 0.2,
			TimeoutS:  15,
		},
		DNS:  DNSProbes{Names: []string{"github.com", "archive.ubuntu.com"}, TimeoutS: 3.0},
		HTTP: HTTPProbes{URLs: []string{"https://github.com"}, TimeoutS: 15, MaxRedirects: 5},
		TCP: TCPProbes{
			Targets:  []TCPTarget{{Host: "github.com", Port: 443}},
			TimeoutS: 3.0,
		},
		Traceroute: TracerouteProbes{MaxHops: 15, WaitS: 1, Queries: 1},
		Ladder: LadderProbes{
			InternetTarget: "8.8.8.8",
			DNSName:        "github.com",
			HTTPSURL:       "https://github.com",
		},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("DefaultProbes() mismatch (-want +got):\n%s", diff)
	}
}

// Why DefaultProbes is a function and not a package var: the slices inside it are
// reachable, so one caller trimming its own copy would otherwise silently rewrite
// what every later caller starts from.
func TestDefaultProbesHandsOutAFreshCopy(t *testing.T) {
	first := DefaultProbes()
	first.Ping.Targets[0] = "192.0.2.1"
	first.TCP.Targets[0].Port = 9999

	second := DefaultProbes()
	if second.Ping.Targets[0] != "8.8.8.8" {
		t.Errorf("ping targets leaked: got %q", second.Ping.Targets[0])
	}
	if second.TCP.Targets[0].Port != 443 {
		t.Errorf("tcp targets leaked: got %d", second.TCP.Targets[0].Port)
	}
}

// The corpus, with every field and every warning pinned. Each want is the measured
// output of v1's resolve_probes over the same bytes, and the warning order is v1's log
// order -- section by section, key by key in the order they are resolved.
func TestLoadProbesFixtures(t *testing.T) {
	tests := []struct {
		name     string
		fixture  string
		mutate   func(*Probes)
		warnings []Warning
	}{
		{
			// Invalid YAML, and a document that is a list rather than a mapping. All
			// three degrade whole rather than partially, and silently: a config file
			// that would not parse is the reader's finding, not the resolver's.
			name:    "unparseable",
			fixture: "probes/malformed.yml",
		},
		{
			name:    "unparseable mapping value",
			fixture: "probes/broken_mapping.yml",
		},
		{
			name:    "top level list",
			fixture: "probes/top_level_list.yml",
		},
		{
			name:    "the documented example",
			fixture: "probes/full_example.yml",
			mutate: func(p *Probes) {
				p.Ping.Targets = []string{"10.0.0.1", "8.8.8.8"}
				p.Ping.Count = 5
				p.Ping.IntervalS = 0.3
				p.DNS.Names = []string{"db.corp.internal", "github.com"}
				p.HTTP.URLs = []string{"https://api.corp.internal/health"}
				p.TCP.Targets = []TCPTarget{
					{Host: "db.corp.internal", Port: 5432},
					{Host: "api.corp.internal", Port: 443},
				}
				p.Traceroute.MaxHops = 20
				p.Ladder = LadderProbes{
					InternetTarget: "9.9.9.9",
					DNSName:        "db.corp.internal",
					HTTPSURL:       "https://api.corp.internal/health",
				}
			},
		},
		{
			// One knob set. Everything else has to stay put -- this is the per-key
			// merge, and getting it wrong turns "shorter ping" into "no dns names".
			name:    "one knob",
			fixture: "probes/partial.yml",
			mutate:  func(p *Probes) { p.Ping.Count = 3 },
		},
		{
			name:    "out of range",
			fixture: "probes/clamped.yml",
			mutate: func(p *Probes) {
				p.Ping.Count = 900
				p.Ping.IntervalS = 0.05
				p.Ping.TimeoutS = 1
				p.DNS.TimeoutS = 30.0
				p.HTTP.TimeoutS = 120
				p.HTTP.MaxRedirects = 0
				p.TCP.TimeoutS = 0.1
				p.Traceroute = TracerouteProbes{MaxHops: 30, WaitS: 1, Queries: 3}
			},
			warnings: []Warning{
				{"ping", "count", WarnClamped, "100000", "900"},
				// 0 arrives as an int and is reported as the float it was compared as.
				{"ping", "interval_s", WarnClamped, "0.0", "0.05"},
				{"ping", "timeout_s", WarnClamped, "0", "1"},
				{"dns", "timeout_s", WarnClamped, "99.0", "30.0"},
				{"http", "timeout_s", WarnClamped, "500", "120"},
				{"http", "max_redirects", WarnClamped, "-3", "0"},
				{"tcp", "timeout_s", WarnClamped, "0.001", "0.1"},
				{"traceroute", "max_hops", WarnClamped, "64", "30"},
				{"traceroute", "wait_s", WarnClamped, "0", "1"},
				{"traceroute", "queries", WarnClamped, "9", "3"},
			},
		},
		{
			// Presence, not truthiness. A host behind an egress filter has to be able
			// to turn a probe off, and `[]` is how it says so.
			name:    "empty lists",
			fixture: "probes/empty_lists.yml",
			mutate: func(p *Probes) {
				p.Ping.Targets = []string{}
				p.DNS.Names = []string{}
				p.HTTP.URLs = []string{}
				p.TCP.Targets = []TCPTarget{}
			},
		},
		{
			// Every slot holding the wrong type. Nothing survives, and the quoted "5"
			// is the one worth staring at: it reads like a number and is refused.
			name:    "junk scalars",
			fixture: "probes/junk_scalars.yml",
			warnings: []Warning{
				{"ping", "targets", WarnNotAList, "'8.8.8.8'", "['8.8.8.8', '1.1.1.1']"},
				{"ping", "count", WarnNotANumber, "'5'", "10"},
				{"ping", "interval_s", WarnNotANumber, "True", "0.2"},
				{
					"dns", "names", WarnNotAList,
					"'github.com'", "['github.com', 'archive.ubuntu.com']",
				},
				{"dns", "timeout_s", WarnNotANumber, "{}", "3.0"},
				{"http", "urls", WarnNotAList, "1", "['https://github.com']"},
				{"http", "max_redirects", WarnNotANumber, "[]", "5"},
				{"tcp", "targets", WarnNotAList, "'db:5432'", "['github.com:443']"},
				{"tcp", "timeout_s", WarnNotANumber, "'3.0'", "3.0"},
				{"traceroute", "max_hops", WarnNotANumber, "False", "15"},
			},
		},
		{
			// The asymmetry that only measurement would have found: a section written
			// as null is absent and silent, while a *list* written as null one level
			// down is present-and-wrong and warns. ping.timeout_s is null here too --
			// also silent, because commenting a knob out is not a mistake.
			name:    "null sections",
			fixture: "probes/null_sections.yml",
			warnings: []Warning{
				{
					"dns", "names", WarnNotAList,
					"None", "['github.com', 'archive.ubuntu.com']",
				},
				{"http", "", WarnNotAMapping, "[]", ""},
				{"tcp", "", WarnNotAMapping, "'github.com:443'", ""},
				{"traceroute", "", WarnNotAMapping, "5", ""},
			},
		},
		{
			// What a hand-edited target list actually contains. Every entry goes
			// through str(), so a bare integer becomes "8080", null becomes "None",
			// and one extra dash -- a list nested in the list -- becomes its Python
			// repr, a target name no resolver will ever answer for. That last one is
			// why str() had to be ported rather than approximated.
			name:    "odd targets",
			fixture: "probes/odd_targets.yml",
			mutate: func(p *Probes) {
				p.Ping.Targets = []string{
					"10.0.0.1", "8080", "True", "None", "['8.8.8.8', '1.1.1.1']",
				}
				p.DNS.Names = []string{"1.5"}
				p.TCP.Targets = []TCPTarget{
					{Host: "db.corp.internal", Port: 5432},
					{Host: "2001:db8::1", Port: 443},
					{Host: "api.corp.internal", Port: 443},
				}
			},
			warnings: []Warning{
				{"tcp", "targets", WarnBadTarget, "'no-port-here'", ""},
				{"tcp", "targets", WarnBadTarget, "'host:notaport'", ""},
				// A bare 8080 is a port with no host, so it is dropped rather than
				// guessed at: probing localhost when the operator meant a database
				// would be confidently wrong.
				{"tcp", "targets", WarnBadTarget, "8080", ""},
			},
		},
		{
			// Numbers past what a machine word holds. The reprs here are the point:
			// they are byte-identical to what v1 logged, which means the comparison
			// happened at full width instead of wrapping through an int64.
			name:    "wide numbers",
			fixture: "probes/wide_numbers.yml",
			mutate: func(p *Probes) {
				p.Ping.Count = 900
				p.Ping.IntervalS = 5.0
				p.DNS.TimeoutS = 0.1
				p.HTTP.MaxRedirects = 0
				p.Traceroute.MaxHops = 30
				p.Traceroute.Queries = 1
			},
			warnings: []Warning{
				{"ping", "count", WarnClamped, "100000000000000000000000", "900"},
				{"ping", "interval_s", WarnClamped, "inf", "5.0"},
				{"dns", "timeout_s", WarnClamped, "-inf", "0.1"},
				{
					"http", "max_redirects", WarnClamped,
					"-10000000000000000303786028427003666890752", "0",
				},
				{
					"traceroute", "max_hops", WarnClamped,
					"1000000000000000019884624838656", "30",
				},
				{"traceroute", "queries", WarnClamped, "-9223372036854775809", "1"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			want := DefaultProbes()
			if tc.mutate != nil {
				tc.mutate(&want)
			}
			got, warnings := LoadProbes(fixture.Path(tc.fixture))
			if diff := cmp.Diff(want, got); diff != "" {
				t.Errorf("LoadProbes(%s) mismatch (-want +got):\n%s", tc.fixture, diff)
			}
			wantWarnings := tc.warnings
			if wantWarnings == nil {
				wantWarnings = []Warning{}
			}
			if diff := cmp.Diff(wantWarnings, warnings); diff != "" {
				t.Errorf("warnings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// A host with no probes.yml is the ordinary case, not a problem to report: an unreadable
// file must not put a line in every default host's report.
func TestLoadProbesWithoutAFile(t *testing.T) {
	got, warnings := LoadProbes(filepath.Join(t.TempDir(), "probes.yml"))
	if diff := cmp.Diff(DefaultProbes(), got); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

func TestResolveProbesWithNothingConfigured(t *testing.T) {
	for name, cfg := range map[string]map[string]any{
		"nil":   nil,
		"empty": {},
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings := ResolveProbes(cfg)
			if diff := cmp.Diff(DefaultProbes(), got); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff([]Warning{}, warnings); diff != "" {
				t.Errorf("warnings mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// The ladder knobs are strings, and a string knob is silent about a wrong type where a
// numeric one warns. That distinction is v1's, and reproducing it keeps the noise floor
// where it was rather than where it arguably should be.
func TestResolveProbesTakesLadderTargetsQuietly(t *testing.T) {
	got, warnings := ResolveProbes(map[string]any{
		"ladder": map[string]any{
			"internet_target": "  9.9.9.9  ",
			"dns_name":        int64(42),
			"https_url":       "\t\n",
		},
	})
	want := LadderProbes{
		InternetTarget: "9.9.9.9",
		DNSName:        "github.com",
		HTTPSURL:       "https://github.com",
	}
	if diff := cmp.Diff(want, got.Ladder); diff != "" {
		t.Errorf("mismatch (-want +got):\n%s", diff)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// The one place Go deliberately does something v1 does not, recorded with the evidence.
//
// Measured on CPython 3.14.6 against the real resolve_probes:
//
//	ping.count = .inf     -> OverflowError: cannot convert float infinity to integer
//	ping.count = .nan     -> ValueError: cannot convert float NaN to integer
//	ping.timeout_s = -.inf -> OverflowError: cannot convert float infinity to integer
//
// So `count: .inf` in probes.yml does not degrade to the default in v1 -- it propagates
// out of load_probes and takes the app down at startup, in the one function whose
// docstring promises "Nothing here raises". Go clamps to the same bound the float knobs
// already clamp to, which is a value the operator was allowed to ask for anyway. A
// diagnostic tool that will not launch because of one line of YAML is worse than one
// that pings 900 times.
//
// The harness sees this too: probes/nonfinite_int_knob.yml carries the `.inf` spelling,
// and py_oracle records a raising case as {"error": {"code": ...}}, so the pair compares
// as a divergence rather than vanishing. It is justified in
// differential/known_divergences.yaml against issue #9. Only `.inf` is in the corpus --
// the other two spellings take the same Go path and differ only in which Python
// exception comes out, which is a detail of the implementation being replaced -- so all
// three stay pinned here.
func TestResolveProbesClampsNonFiniteIntegerKnobsWhereV1Crashes(t *testing.T) {
	tests := []struct {
		name string
		v    float64
		want int64
	}{
		{"nan", math.NaN(), 900},
		{"infinity", math.Inf(1), 900},
		{"negative infinity", math.Inf(-1), 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := ResolveProbes(map[string]any{
				"ping": map[string]any{"count": tc.v},
			})
			if got.Ping.Count != tc.want {
				t.Errorf("count = %d, want %d", got.Ping.Count, tc.want)
			}
			if len(warnings) != 1 || warnings[0].Kind != WarnClamped {
				t.Fatalf("warnings = %+v, want one clamp", warnings)
			}
		})
	}
}

// The same defect on the other side of the type split: v1 raises OverflowError from
// float(value) when an integer knob holds an integer too wide for a float64, measured as
//
//	ping.interval_s = 10**400 -> OverflowError: int too large to convert to float
//
// Go takes it to infinity, which the clamp already handles.
//
// Corpus case: probes/unrepresentable_float_knob.yml, justified against issue #9. It
// spells the magnitude as a 401-digit integer rather than 1.0e+400 on purpose --
// PyYAML resolves the float literal to inf without ever calling float(int), which is
// why probes.wide_numbers agrees on both sides and does not cover this.
func TestResolveProbesClampsUnrepresentableIntegersOnFloatKnobsWhereV1Crashes(t *testing.T) {
	huge := new(big.Int).Exp(big.NewInt(10), big.NewInt(400), nil)
	for name, v := range map[string]*big.Int{
		"positive": huge,
		"negative": new(big.Int).Neg(huge),
	} {
		t.Run(name, func(t *testing.T) {
			got, warnings := ResolveProbes(map[string]any{
				"ping": map[string]any{"interval_s": v},
			})
			want := 5.0
			if v.Sign() < 0 {
				want = 0.05
			}
			if got.Ping.IntervalS != want {
				t.Errorf("interval_s = %v, want %v", got.Ping.IntervalS, want)
			}
			if len(warnings) != 1 || warnings[0].Kind != WarnClamped {
				t.Fatalf("warnings = %+v, want one clamp", warnings)
			}
		})
	}
}

// An integer knob holding a *small* integer of unbounded width still has to come out
// right, which is the case a naive Cmp-free port gets wrong in the quiet direction.
func TestResolveProbesAcceptsAnInRangeWideInteger(t *testing.T) {
	got, warnings := ResolveProbes(map[string]any{
		"ping": map[string]any{"count": big.NewInt(7)},
	})
	if got.Ping.Count != 7 {
		t.Errorf("count = %d, want 7", got.Ping.Count)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// A float on an integer knob truncates toward zero rather than rounding, and a value
// that truncates back into range is not a clamp and must not warn.
func TestResolveProbesTruncatesFloatsOnIntegerKnobs(t *testing.T) {
	tests := []struct {
		name      string
		v         float64
		want      int64
		wantWarns int
	}{
		{"just under the top", 900.9, 900, 0},
		{"toward zero", 3.9, 3, 0},
		{"toward zero from below", -3.9, 1, 1},
		{"out of range", 901.2, 900, 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, warnings := ResolveProbes(map[string]any{
				"ping": map[string]any{"count": tc.v},
			})
			if got.Ping.Count != tc.want {
				t.Errorf("count = %d, want %d", got.Ping.Count, tc.want)
			}
			if len(warnings) != tc.wantWarns {
				t.Errorf("warnings = %+v, want %d", warnings, tc.wantWarns)
			}
		})
	}
}

// Python's max(low, min(high, v)) with the comparison order intact, which is the whole
// reason it is transcribed rather than case-analysed. Go's own min/max builtins
// propagate NaN and would give nan where Python gives high.
func TestClampFloat(t *testing.T) {
	bounds := floatRange{0.05, 5.0}
	tests := []struct {
		name string
		v    float64
		want float64
	}{
		{"inside", 0.3, 0.3},
		{"at the bottom", 0.05, 0.05},
		{"at the top", 5.0, 5.0},
		{"below", 0.0, 0.05},
		{"above", 9.0, 5.0},
		// Measured against CPython: nan and +inf both land on high because neither is
		// less than high, and -inf lands on low.
		{"nan", math.NaN(), 5.0},
		{"infinity", math.Inf(1), 5.0},
		{"negative infinity", math.Inf(-1), 0.05},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := clampFloat(tc.v, bounds); got != tc.want {
				t.Errorf("clampFloat(%v) = %v, want %v", tc.v, got, tc.want)
			}
		})
	}
}

// A warning names the value the operator would have to type to get it back, not the
// Go or Python spelling of the container it landed in.
func TestWarningsRenderDefaultsTheWayProbesYAMLWritesThem(t *testing.T) {
	tests := []struct {
		name string
		v    any
		want string
	}{
		{"strings", []string{"a", "b"}, "['a', 'b']"},
		{"no strings", []string{}, "[]"},
		{"targets", []TCPTarget{{Host: "github.com", Port: 443}}, "['github.com:443']"},
		{
			"a bracketed target",
			[]TCPTarget{{Host: "2001:db8::1", Port: 443}},
			"['[2001:db8::1]:443']",
		},
		{"anything else", int64(5), "5"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := repr(tc.v); got != tc.want {
				t.Errorf("repr(%#v) = %q, want %q", tc.v, got, tc.want)
			}
		})
	}
}
