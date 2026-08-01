// Package threshold grades a measured number into ok, warn or crit.
//
// In v1 that judgement lived in nine places. Two of them were module constants
// (disk.usage.WARN_PCT/CRITICAL_PCT and the identical pair in disk.inodes) and
// seven were private functions in screens/dashboard.py -- which put the fleet's
// severity policy inside the Textual layer, where nothing headless could reach
// it. Three consumers need the same answer: the TUI's colours, the trips[] array
// in `check --json`, and the agent's threshold-trip events. One grader is what
// keeps them from drifting.
//
// Every bound here was read off v1 rather than chosen. Where this package changes
// v1's behaviour, it says so at the site.
package threshold

import (
	"fmt"
	"math"
	"sort"
)

// Severity is how bad a graded value is.
//
// The three values are the gradeable subset of the report's status enum: skipped,
// unavailable and error describe a check that did not produce a number, so no
// rule can return them. Ordered so that a worst-of aggregation is a comparison.
type Severity int

// OK, Warn and Crit are the three gradeable severities, ascending.
const (
	OK Severity = iota
	Warn
	Crit
)

// String is the wire spelling. These strings are part of the public JSON
// contract -- `status` is a frozen closed enum -- so they are not display text
// and must not be prettified.
func (s Severity) String() string {
	switch s {
	case OK:
		return "ok"
	case Warn:
		return "warn"
	case Crit:
		return "crit"
	default:
		return fmt.Sprintf("severity(%d)", int(s))
	}
}

// Worse returns the more severe of two severities, so a check with several
// graded values reports the worst rather than the last.
func Worse(a, b Severity) Severity {
	if b > a {
		return b
	}
	return a
}

// A Rule is one ascending ladder: at or above Warn is a warning, at or above
// Crit is critical, and anything below Warn is ok.
//
// Ascending only, because all seven of v1's ladders are -- load per CPU, memory
// used, temperature, security updates, disk used, inodes used, failed services.
// Every one of them counts a quantity where more is worse. A descending rule
// (free bytes below a floor) is a Direction field away, and adding it later is
// additive; inventing the abstraction now would mean guessing at a shape nothing
// in the port needs.
//
// The comparison is >= at both bounds, matching v1. Four of the seven sites spell
// the warn bound as `> 0` rather than `>= 1`; every one of those grades an
// integer count, where the two are the same predicate. Rules that grade a count
// therefore carry Warn: 1.
type Rule struct {
	// ID is the stable name an operator writes in thresholds.yml and a trip
	// reports. Dotted, like a check ID, and retired rather than repurposed.
	ID string

	// Warn and Crit are the two bounds, in the unit named by Unit.
	Warn float64
	Crit float64

	// Unit labels the number for the operator: "%", "C", "count", "ratio".
	// Formatting is internal/render's job; this is only what the number means.
	Unit string

	// What the rule measures, for `fleetfix doctor` and thresholds.yml comments.
	Description string
}

// Grade places v on the ladder.
//
// A NaN grades ok, which is v1's behaviour rather than a decision made here: every
// comparison against NaN is false, so Python fell through to the empty-severity
// return exactly as this falls through to OK. It is worth knowing that a check
// which cannot compute its number must report unavailable rather than hand NaN to
// a grader -- the grader cannot tell those apart and will call it healthy.
func (r Rule) Grade(v float64) Severity {
	switch {
	case v >= r.Crit:
		return Crit
	case v >= r.Warn:
		return Warn
	default:
		return OK
	}
}

// Trip is a rule that fired: what was measured, against which bound.
//
// Carries the bound as well as the value because an alert that says only "82%"
// makes the reader go and find the configuration to learn whether that is bad.
type Trip struct {
	Rule     string   `json:"rule"`
	Severity Severity `json:"-"`
	Status   string   `json:"status"`
	Value    float64  `json:"value"`
	Bound    float64  `json:"bound"`
	Unit     string   `json:"unit"`
	// Subject names what was measured where a rule grades more than one thing --
	// a mount point, an interface, a thermal zone. Empty for a host-wide rule.
	Subject string `json:"subject"`
}

// Check grades v and returns a Trip when the rule fires.
//
// The bool rather than a nil *Trip: a trip is a value, and returning a pointer
// invites a caller to keep one past the slice it came from.
func (r Rule) Check(v float64, subject string) (Trip, bool) {
	sev := r.Grade(v)
	if sev == OK {
		return Trip{}, false
	}
	bound := r.Warn
	if sev == Crit {
		bound = r.Crit
	}
	return Trip{
		Rule:     r.ID,
		Severity: sev,
		Status:   sev.String(),
		Value:    v,
		Bound:    bound,
		Unit:     r.Unit,
		Subject:  subject,
	}, true
}

// Validate reports what is wrong with a rule an operator wrote.
//
// A rule is not rejected for being unusual -- a fleet may genuinely want to warn
// at 50% -- only for being unreachable. Crit below Warn makes the warn band empty:
// every value that would have warned is already critical, so the operator has
// silently turned their warning tier off. That is worth a config warning rather
// than a refusal, because refusing means a typo in one bound takes the whole
// check offline, and a fleet tool that stops reporting is worse than one
// reporting too loudly.
func (r Rule) Validate() error {
	switch {
	case r.ID == "":
		return fmt.Errorf("threshold rule has no id")
	case math.IsNaN(r.Warn) || math.IsNaN(r.Crit):
		return fmt.Errorf("%s: a bound is NaN, so nothing can ever cross it", r.ID)
	case r.Crit < r.Warn:
		return fmt.Errorf("%s: crit=%g is below warn=%g, so the warn band is empty", r.ID, r.Crit, r.Warn)
	default:
		return nil
	}
}

// A Set is the host's grading policy: every rule, by id.
type Set map[string]Rule

// Rule ids. Constants rather than bare strings because these are the keys an
// operator writes in thresholds.yml and the names that appear in trips[]; a
// typo in one should not compile.
const (
	LoadPerCPU     = "cpu.load_per_cpu"
	MemUsedPct     = "mem.used_pct"
	ThermalTempC   = "thermal.temp_c"
	UpdatesSecure  = "updates.security"
	DiskUsedPct    = "disk.used_pct"
	DiskInodePct   = "disk.inode_pct"
	ServicesFailed = "services.failed"
)

// Defaults is v1's policy, measured from the source rather than reconstructed.
//
// Sites, so a future change can be checked against what it replaces:
//
//	cpu.load_per_cpu  screens/dashboard.py:87  _load_severity, load.one / max(cpu_count, 1)
//	mem.used_pct      screens/dashboard.py:96  _mem_severity
//	thermal.temp_c    screens/dashboard.py:104 _temp_severity
//	updates.security  screens/dashboard.py:112 _updates_severity, `> 0` warn
//	disk.used_pct     modules/disk/usage.py:18 WARN_PCT / CRITICAL_PCT
//	disk.inode_pct    modules/disk/inodes.py:18 the same pair, separately declared
//	services.failed   screens/dashboard.py:136 _services_severity, `> 0` warn
//
// disk and inodes get separate rules even though v1 gives them identical numbers.
// They were separate constants in separate modules, an operator may well want a
// tighter inode bound than a byte bound, and collapsing them would be a policy
// change smuggled in as tidying.
func Defaults() Set {
	return Set{
		LoadPerCPU: {
			ID: LoadPerCPU, Warn: 1.0, Crit: 2.0, Unit: "ratio",
			Description: "one-minute load average divided by CPU count",
		},
		MemUsedPct: {
			ID: MemUsedPct, Warn: 80, Crit: 95, Unit: "%",
			Description: "memory in use",
		},
		ThermalTempC: {
			ID: ThermalTempC, Warn: 70, Crit: 85, Unit: "C",
			Description: "hottest thermal zone",
		},
		UpdatesSecure: {
			ID: UpdatesSecure, Warn: 1, Crit: 5, Unit: "count",
			Description: "pending security updates",
		},
		DiskUsedPct: {
			ID: DiskUsedPct, Warn: 85, Crit: 95, Unit: "%",
			Description: "filesystem capacity used",
		},
		DiskInodePct: {
			ID: DiskInodePct, Warn: 85, Crit: 95, Unit: "%",
			Description: "filesystem inodes used",
		},
		ServicesFailed: {
			ID: ServicesFailed, Warn: 1, Crit: 3, Unit: "count",
			Description: "systemd units in the failed state",
		},
	}
}

// Get returns the rule, or a zero rule and false.
//
// A caller that grades against a missing rule would grade against Warn: 0 and
// Crit: 0, which marks every value critical -- so this does not fall back, and
// the caller has to decide what a missing rule means. Merge makes that case
// unreachable for any rule in Defaults.
func (s Set) Get(id string) (Rule, bool) {
	r, ok := s[id]
	return r, ok
}

// Grade is the shorthand for a check that has one number and one rule. An unknown
// id grades ok rather than crit, because a rule nobody defined is not evidence of
// a problem; the missing rule surfaces through Merge's warnings instead.
func (s Set) Grade(id string, v float64) Severity {
	r, ok := s[id]
	if !ok {
		return OK
	}
	return r.Grade(v)
}

// Merge layers an operator's overrides onto the defaults and returns the merged
// set alongside a warning per override that could not be applied.
//
// Never an error, and never a partial set: this feeds config_warnings[] in the
// report, and the contract for the whole config layer is that a bad file degrades
// rather than takes the host's monitoring offline. An override for an unknown id
// is reported rather than kept, since silently accepting it would let a typo look
// like a configured policy that never fires.
//
// Overrides carry only bounds. ID, Unit and Description come from the default,
// so an operator cannot rename a rule out from under the consumers that pin on it
// or relabel a percentage as a temperature.
func Merge(overrides map[string]Rule) (Set, []string) {
	set := Defaults()
	var warnings []string

	// Sorted, so two runs over the same file produce byte-identical warnings --
	// the report is asserted byte-stable across consecutive runs.
	ids := make([]string, 0, len(overrides))
	for id := range overrides {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		base, known := set[id]
		if !known {
			warnings = append(warnings, fmt.Sprintf(
				"thresholds.yml: no rule named %q, ignoring it", id,
			))
			continue
		}
		merged := base
		merged.Warn = overrides[id].Warn
		merged.Crit = overrides[id].Crit
		if err := merged.Validate(); err != nil {
			warnings = append(warnings, fmt.Sprintf(
				"thresholds.yml: %v, keeping the default warn=%g crit=%g",
				err, base.Warn, base.Crit,
			))
			continue
		}
		set[id] = merged
	}
	return set, warnings
}

// IDs returns every rule id in sorted order, for `fleetfix doctor` and for tests
// that must enumerate the policy rather than restate it.
func (s Set) IDs() []string {
	ids := make([]string, 0, len(s))
	for id := range s {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
