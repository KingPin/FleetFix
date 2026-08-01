package network

// Per-host network probe targets, from probes.yml.
//
// The file is entirely optional: DefaultProbes is a deliberately boring set of public
// targets, so a freshly imaged host with no config is fully functional and probes.yml
// is only for saying "on *this* box, check these instead".
//
// Nothing here fails. A missing file, invalid YAML, a top-level list, a junk scalar,
// or an out-of-range number all degrade to the default for that one field -- a broken
// config file must never be the reason the Network view will not open.
//
// The loader lives here rather than in internal/config, mirroring v1: the schema is
// richer than anything there and it needs ParseHostPort. Importing internal/config
// the other way is what v1 does too (modules/disk/blacklist.py already did); the
// boundary rule is "no TUI", not "no config".

import (
	"math"
	"math/big"
	"slices"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// PingProbes is the ping section: which hosts, how many packets, how fast.
type PingProbes struct {
	Targets   []string `json:"targets"`
	Count     int64    `json:"count"`
	IntervalS float64  `json:"interval_s"`
	TimeoutS  int64    `json:"timeout_s"`
}

// DNSProbes is the dns section.
type DNSProbes struct {
	Names    []string `json:"names"`
	TimeoutS float64  `json:"timeout_s"`
}

// HTTPProbes is the http section.
type HTTPProbes struct {
	URLs         []string `json:"urls"`
	TimeoutS     int64    `json:"timeout_s"`
	MaxRedirects int64    `json:"max_redirects"`
}

// TCPProbes is the tcp section.
type TCPProbes struct {
	Targets  []TCPTarget `json:"targets"`
	TimeoutS float64     `json:"timeout_s"`
}

// TracerouteProbes is the traceroute section.
type TracerouteProbes struct {
	MaxHops int64 `json:"max_hops"`
	WaitS   int64 `json:"wait_s"`
	Queries int64 `json:"queries"`
}

// LadderProbes is the single target per layer that the connectivity ladder walks.
type LadderProbes struct {
	InternetTarget string `json:"internet_target"`
	DNSName        string `json:"dns_name"`
	HTTPSURL       string `json:"https_url"`
}

// Probes is a resolved probes.yml: every knob the network checks read.
type Probes struct {
	Ping       PingProbes       `json:"ping"`
	DNS        DNSProbes        `json:"dns"`
	HTTP       HTTPProbes       `json:"http"`
	TCP        TCPProbes        `json:"tcp"`
	Traceroute TracerouteProbes `json:"traceroute"`
	Ladder     LadderProbes     `json:"ladder"`
}

// WarningKind names what probes.yml got wrong, without spelling the prose.
//
// A code rather than a message because v1 only ever logged these, so there is no
// wording to be faithful to -- and because M3 puts them in the report's
// config_warnings[], where a caller needs the section and key as data rather than
// embedded in a sentence.
type WarningKind string

const (
	// WarnNotAMapping is a section that is not a mapping, ignored whole.
	WarnNotAMapping WarningKind = "not_a_mapping"
	// WarnNotAList is a target list that is not a list, replaced by the default.
	WarnNotAList WarningKind = "not_a_list"
	// WarnNotANumber is a numeric knob holding something that is not a number.
	WarnNotANumber WarningKind = "not_a_number"
	// WarnClamped is a number outside the range that becomes a subprocess argument.
	WarnClamped WarningKind = "clamped"
	// WarnBadTarget is a tcp target with no determinable port, dropped.
	WarnBadTarget WarningKind = "bad_target"
)

// Warning is one thing probes.yml said that was adjusted or ignored.
//
// Got and Used are Python reprs so that a value's type survives into the message: an
// operator who wrote `count: "5"` needs to see the quotes to understand why their
// number was refused. Key is empty when the whole section was the problem, and Used
// is empty when the value was dropped rather than replaced.
type Warning struct {
	Section string      `json:"section"`
	Key     string      `json:"key"`
	Kind    WarningKind `json:"kind"`
	Got     string      `json:"got"`
	Used    string      `json:"used"`
}

// Clamps exist because these numbers become subprocess arguments. `ping.count: 100000`
// with `interval_s: 0` is a self-inflicted DoS on the operator's own box, and a
// fat-fingered config should not be able to cause one.
var (
	countRange        = intRange{1, 900}
	pingIntervalRange = floatRange{0.05, 5.0}
	pingTimeoutRange  = intRange{1, 300}
	dnsTimeoutRange   = floatRange{0.1, 30.0}
	httpTimeoutRange  = intRange{1, 120}
	redirectRange     = intRange{0, 20}
	tcpTimeoutRange   = floatRange{0.1, 30.0}
	maxHopsRange      = intRange{1, 30}
	traceWaitRange    = intRange{1, 10}
	traceQueriesRange = intRange{1, 3}
)

type intRange struct{ low, high int64 }

type floatRange struct{ low, high float64 }

// DefaultProbes is the built-in target set, and is a function rather than a variable
// because Probes holds slices: a package-level value would let one caller's append
// rewrite every later caller's defaults.
func DefaultProbes() Probes {
	return Probes{
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
}

// ResolveProbes merges a probes.yml mapping over DefaultProbes. Never fails.
//
// Scalar knobs merge per-key; target lists replace wholesale. The asymmetry is
// deliberate. A list is a fleet inventory, not a suggestion: appending our github.com
// to an operator's [api.internal, db.internal] would put a permanently-red row on an
// egress-filtered host, and a check that is always red trains operators to ignore
// red. Scalars are independent knobs, so `ping: {count: 3}` means "shorter ping", not
// "also zero the interval".
//
// The escape hatch is presence, not truthiness: `targets: []` is an explicit "no
// presets on this box" and is honoured as empty. Note that `targets: null` is not --
// it is present and not a list, so it warns and keeps the defaults, where a whole
// section set to null is silently absent. That asymmetry is v1's and is measured.
func ResolveProbes(cfg map[string]any) (Probes, []Warning) {
	r := &resolver{warnings: []Warning{}}
	def := DefaultProbes()

	ping := r.section(cfg, "ping")
	pingProbes := PingProbes{
		Targets:   r.strs(ping, "ping", "targets", def.Ping.Targets),
		Count:     r.intVal(ping, "ping", "count", def.Ping.Count, countRange),
		IntervalS: r.floatVal(ping, "ping", "interval_s", def.Ping.IntervalS, pingIntervalRange),
		TimeoutS:  r.intVal(ping, "ping", "timeout_s", def.Ping.TimeoutS, pingTimeoutRange),
	}

	dns := r.section(cfg, "dns")
	dnsProbes := DNSProbes{
		Names:    r.strs(dns, "dns", "names", def.DNS.Names),
		TimeoutS: r.floatVal(dns, "dns", "timeout_s", def.DNS.TimeoutS, dnsTimeoutRange),
	}

	httpCfg := r.section(cfg, "http")
	httpProbes := HTTPProbes{
		URLs:     r.strs(httpCfg, "http", "urls", def.HTTP.URLs),
		TimeoutS: r.intVal(httpCfg, "http", "timeout_s", def.HTTP.TimeoutS, httpTimeoutRange),
		MaxRedirects: r.intVal(
			httpCfg, "http", "max_redirects", def.HTTP.MaxRedirects, redirectRange,
		),
	}

	tcp := r.section(cfg, "tcp")
	tcpProbes := TCPProbes{
		Targets:  r.tcpTargets(tcp, def.TCP.Targets),
		TimeoutS: r.floatVal(tcp, "tcp", "timeout_s", def.TCP.TimeoutS, tcpTimeoutRange),
	}

	trace := r.section(cfg, "traceroute")
	traceProbes := TracerouteProbes{
		MaxHops: r.intVal(trace, "traceroute", "max_hops", def.Traceroute.MaxHops, maxHopsRange),
		WaitS:   r.intVal(trace, "traceroute", "wait_s", def.Traceroute.WaitS, traceWaitRange),
		Queries: r.intVal(
			trace, "traceroute", "queries", def.Traceroute.Queries, traceQueriesRange,
		),
	}

	ladder := r.section(cfg, "ladder")
	ladderProbes := LadderProbes{
		InternetTarget: r.str(ladder, "internet_target", def.Ladder.InternetTarget),
		DNSName:        r.str(ladder, "dns_name", def.Ladder.DNSName),
		HTTPSURL:       r.str(ladder, "https_url", def.Ladder.HTTPSURL),
	}

	return Probes{
		Ping:       pingProbes,
		DNS:        dnsProbes,
		HTTP:       httpProbes,
		TCP:        tcpProbes,
		Traceroute: traceProbes,
		Ladder:     ladderProbes,
	}, r.warnings
}

// LoadProbes reads and resolves probes.yml, which is what the app does once at
// startup. An unreadable or unparseable file resolves to the defaults.
func LoadProbes(path string) (Probes, []Warning) {
	// The read error is dropped rather than warned about, because it is the ordinary
	// case: no probes.yml is what a host without one has, and reporting it as a
	// config problem would put a line in every default host's report.
	cfg, _ := config.ReadProbesYAML(path)
	return ResolveProbes(cfg)
}

// resolver accumulates the warnings that merging produced.
type resolver struct{ warnings []Warning }

// warn records a value that was replaced by another one.
func (r *resolver) warn(section, key string, kind WarningKind, got, used any) {
	r.record(section, key, kind, repr(got), repr(used))
}

// drop records a value that was thrown away, so nothing took its place.
func (r *resolver) drop(section, key string, kind WarningKind, got any) {
	r.record(section, key, kind, repr(got), "")
}

func (r *resolver) record(section, key string, kind WarningKind, got, used string) {
	r.warnings = append(r.warnings, Warning{
		Section: section,
		Key:     key,
		Kind:    kind,
		Got:     got,
		Used:    used,
	})
}

// repr is config.PyRepr plus the two slice types this package's defaults are held in,
// which Python would have shown as a tuple of dataclasses. Targets render the way the
// operator writes them in YAML instead -- "['github.com:443']" is the line they would
// have to type to get that value back, where "(TcpTarget(host='github.com', port=443),)"
// is a Python detail leaking into a message about their config file.
func repr(v any) string {
	switch t := v.(type) {
	case []string:
		return config.PyRepr(anySlice(t))
	case []TCPTarget:
		out := make([]any, len(t))
		for i, target := range t {
			out[i] = target.String()
		}
		return config.PyRepr(out)
	default:
		return config.PyRepr(v)
	}
}

func anySlice[T any](in []T) []any {
	out := make([]any, len(in))
	for i, v := range in {
		out[i] = v
	}
	return out
}

// section is one section of the config, or nil when it is not a mapping.
//
// An absent section and one written as null are the same thing and warn about
// nothing, because v1 reaches both through .get() and tests the result for None.
func (r *resolver) section(cfg map[string]any, key string) map[string]any {
	v := cfg[key]
	if v == nil {
		return nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		r.drop(key, "", WarnNotAMapping, v)
		return nil
	}
	return m
}

// strs is a list of strings, replacing the default wholesale when present.
func (r *resolver) strs(section map[string]any, sec, key string, fallback []string) []string {
	v, present := section[key]
	if !present {
		return slices.Clone(fallback)
	}
	list, ok := v.([]any)
	if !ok {
		r.warn(sec, key, WarnNotAList, v, fallback)
		return slices.Clone(fallback)
	}
	// str() then strip, dropping whatever is left empty. The str() is why a bare
	// number in a target list becomes "8080" rather than being refused, and why one
	// extra dash -- a list inside the list -- becomes the literal "['8.8.8.8']".
	out := []string{}
	for _, item := range list {
		if s := strings.TrimFunc(config.PyStr(item), pytext.IsSpace); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// tcpTargets is strs plus a host:port parse, dropping what has no port.
func (r *resolver) tcpTargets(section map[string]any, fallback []TCPTarget) []TCPTarget {
	v, present := section["targets"]
	if !present {
		return slices.Clone(fallback)
	}
	list, ok := v.([]any)
	if !ok {
		r.warn("tcp", "targets", WarnNotAList, v, fallback)
		return slices.Clone(fallback)
	}
	out := []TCPTarget{}
	for _, item := range list {
		// No default port: dropped rather than guessed, because probing 80 when the
		// operator meant 5432 would be confidently wrong.
		target, ok := ParseHostPort(config.PyStr(item), nil)
		if !ok {
			r.drop("tcp", "targets", WarnBadTarget, item)
			continue
		}
		out = append(out, target)
	}
	return out
}

// str is a single string knob.
//
// Silent on a wrong type, unlike the numeric knobs: v1 warns about `count: "5"` and
// says nothing about `dns_name: 42`. Reproduced because the noise floor is the point
// of the distinction being visible at all -- if it is wrong, it is wrong in v1 too.
func (r *resolver) str(section map[string]any, key, fallback string) string {
	s, _ := section[key].(string)
	if trimmed := strings.TrimFunc(s, pytext.IsSpace); trimmed != "" {
		return trimmed
	}
	return fallback
}

// intVal is an integer knob, truncated toward zero and clamped into bounds.
//
// Go gets one check for free that v1 had to write out: bool is not an int here, so
// `count: true` is junk rather than 1 without a special case.
func (r *resolver) intVal(
	section map[string]any, sec, key string, fallback int64, bounds intRange,
) int64 {
	switch n := section[key].(type) {
	case int64:
		c := max(bounds.low, min(bounds.high, n))
		if c != n {
			r.warn(sec, key, WarnClamped, n, c)
		}
		return c
	case *big.Int:
		return r.clampBig(sec, key, n, bounds)
	case float64:
		// int() of a non-finite float is where v1 stops being a program: it raises
		// OverflowError on inf and ValueError on nan, so one line of YAML takes the
		// whole app down at startup rather than degrading to a default. Clamping is
		// the same answer the float knobs already give (max(low, min(high, v)) sends
		// nan and +inf to high, -inf to low) and lands on a number the operator was
		// allowed to ask for anyway.
		if math.IsNaN(n) || math.IsInf(n, 0) {
			c := int64(clampFloat(n, floatRange{float64(bounds.low), float64(bounds.high)}))
			r.warn(sec, key, WarnClamped, n, c)
			return c
		}
		// int(float) truncates toward zero and is exact at any magnitude, so the
		// clamp happens in big.Int space: int64(1e30) is undefined in Go where
		// Python's int(1e30) is a 31-digit number, and the warning should quote the
		// number the operator wrote.
		t, _ := big.NewFloat(n).Int(nil)
		return r.clampBig(sec, key, t, bounds)
	default:
		r.warnNotANumber(section, sec, key, fallback)
		return fallback
	}
}

// clampBig clamps an integer of unbounded width. The comparison has to happen before
// any narrowing -- 10**23 clamps to 900, it does not wrap.
func (r *resolver) clampBig(sec, key string, n *big.Int, bounds intRange) int64 {
	if n.Cmp(big.NewInt(bounds.low)) < 0 {
		r.warn(sec, key, WarnClamped, n, bounds.low)
		return bounds.low
	}
	if n.Cmp(big.NewInt(bounds.high)) > 0 {
		r.warn(sec, key, WarnClamped, n, bounds.high)
		return bounds.high
	}
	return n.Int64()
}

// floatVal is a float knob, clamped into bounds.
func (r *resolver) floatVal(
	section map[string]any, sec, key string, fallback float64, bounds floatRange,
) float64 {
	var f float64
	switch n := section[key].(type) {
	case int64:
		f = float64(n)
	case *big.Int:
		// float() of an int too big for a float64 is v1's other startup crash --
		// OverflowError, not inf. Go takes it to inf and lets the clamp put it at the
		// top of the range, same reasoning as the non-finite int path above.
		f, _ = new(big.Float).SetInt(n).Float64()
	case float64:
		f = n
	default:
		r.warnNotANumber(section, sec, key, fallback)
		return fallback
	}
	c := clampFloat(f, bounds)
	if c != f {
		r.warn(sec, key, WarnClamped, f, c)
	}
	return c
}

// warnNotANumber is the shared "that is not a number" report.
//
// Silent when the key is absent or explicitly null, which is v1's `if value is not
// None` -- an operator who commented a knob out is not making a mistake.
func (r *resolver) warnNotANumber(section map[string]any, sec, key string, fallback any) {
	if v := section[key]; v != nil {
		r.warn(sec, key, WarnNotANumber, v, fallback)
	}
}

// clampFloat is Python's max(low, min(high, v)), transcribed rather than reasoned
// about, because the non-finite answers fall out of the comparison order: NaN and
// +Inf both land on high (neither is < high), and -Inf lands on low. Measured, not
// deduced -- `interval_s: .nan` resolves to 5.0 in v1.
//
// Go's own min/max builtins would not do: they propagate NaN, so max(0.05,
// min(5.0, nan)) is nan in Go and 5.0 in Python.
func clampFloat(v float64, bounds floatRange) float64 {
	m := bounds.high
	if v < bounds.high {
		m = v
	}
	if m > bounds.low {
		return m
	}
	return bounds.low
}
