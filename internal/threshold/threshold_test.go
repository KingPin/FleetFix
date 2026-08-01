package threshold

import (
	"math"
	"strings"
	"testing"
)

// TestDefaultsGradeWhatV1Graded is the measurement, not a restatement.
//
// The expected column was produced by importing v1's own severity functions --
// screens/dashboard.py's seven _*_severity helpers, with the disk pair reading
// disk.usage.WARN_PCT/CRITICAL_PCT -- and calling them over this grid on the
// interpreter the release binary bundles. "" is v1's ok (an empty CSS class),
// metric-warn is warn, metric-critical is crit.
//
// The grid straddles every bound by 0.1 in both directions, because the whole
// content of a ladder is which side of >= a boundary value lands on. A table
// that only sampled 50 and 99 would pass against >, >=, and a bound one point
// out.
func TestDefaultsGradeWhatV1Graded(t *testing.T) {
	set := Defaults()
	tests := []struct {
		rule string
		v    float64
		want Severity
	}{
		// cpu.load_per_cpu: the ratio, already divided. warn 1.0, crit 2.0.
		{LoadPerCPU, 0.0, OK},
		{LoadPerCPU, 0.5, OK},
		{LoadPerCPU, 0.99, OK},
		{LoadPerCPU, 1.0, Warn},
		{LoadPerCPU, 1.5, Warn},
		{LoadPerCPU, 1.99, Warn},
		{LoadPerCPU, 2.0, Crit},
		{LoadPerCPU, 2.5, Crit},
		{LoadPerCPU, 100.0, Crit},

		// mem.used_pct: warn 80, crit 95. The one ladder whose warn bound is not
		// shared with the disk pair, which is why it gets its own rule.
		{MemUsedPct, -1, OK},
		{MemUsedPct, 0, OK},
		{MemUsedPct, 79.9, OK},
		{MemUsedPct, 80, Warn},
		{MemUsedPct, 85, Warn},
		{MemUsedPct, 94.9, Warn},
		{MemUsedPct, 95, Crit},
		{MemUsedPct, 100, Crit},
		{MemUsedPct, 101, Crit},

		// thermal.temp_c: warn 70, crit 85. Negative is a real reading, not an
		// error -- an ambient sensor below zero is ok, and grading it otherwise
		// would page someone about a cold room.
		{ThermalTempC, -10, OK},
		{ThermalTempC, 40, OK},
		{ThermalTempC, 69.9, OK},
		{ThermalTempC, 70, Warn},
		{ThermalTempC, 84.9, Warn},
		{ThermalTempC, 85, Crit},
		{ThermalTempC, 90, Crit},
		{ThermalTempC, 200, Crit},

		// updates.security: v1 spells the warn arm `security > 0`. On the integer
		// count it grades, that is Warn: 1 -- and 0.5 pending updates is not a
		// state the collector can produce.
		{UpdatesSecure, 0, OK},
		{UpdatesSecure, 1, Warn},
		{UpdatesSecure, 2, Warn},
		{UpdatesSecure, 3, Warn},
		{UpdatesSecure, 4, Warn},
		{UpdatesSecure, 5, Crit},
		{UpdatesSecure, 6, Crit},
		{UpdatesSecure, 100, Crit},

		// disk.used_pct and disk.inode_pct: warn 85, crit 95, declared separately
		// in v1 and kept separate here.
		{DiskUsedPct, -1, OK},
		{DiskUsedPct, 84.9, OK},
		{DiskUsedPct, 85, Warn},
		{DiskUsedPct, 94.9, Warn},
		{DiskUsedPct, 95, Crit},
		{DiskUsedPct, 100, Crit},
		{DiskUsedPct, 101, Crit},
		{DiskInodePct, -1, OK},
		{DiskInodePct, 84.9, OK},
		{DiskInodePct, 85, Warn},
		{DiskInodePct, 94.9, Warn},
		{DiskInodePct, 95, Crit},
		{DiskInodePct, 100, Crit},

		// services.failed: `count > 0` warn, `count >= 3` crit. Note the gap --
		// two failed units warn, three are critical.
		{ServicesFailed, 0, OK},
		{ServicesFailed, 1, Warn},
		{ServicesFailed, 2, Warn},
		{ServicesFailed, 3, Crit},
		{ServicesFailed, 100, Crit},
	}
	for _, tt := range tests {
		rule, ok := set.Get(tt.rule)
		if !ok {
			t.Errorf("no rule %q", tt.rule)
			continue
		}
		if got := rule.Grade(tt.v); got != tt.want {
			t.Errorf("%s.Grade(%g) = %s, v1 says %s", tt.rule, tt.v, got, tt.want)
		}
		// The Set shorthand has to agree with the rule it looked up.
		if got := set.Grade(tt.rule, tt.v); got != rule.Grade(tt.v) {
			t.Errorf("Set.Grade(%s, %g) = %s, Rule.Grade = %s", tt.rule, tt.v, got, rule.Grade(tt.v))
		}
	}
}

// A NaN grades ok, and this pins it because it is the one answer here that is
// dangerous rather than merely correct. v1 does the same thing for the same
// reason -- every comparison against NaN is false, so both fall through -- so
// reproducing it is right, but a check whose number came out NaN has not
// measured a healthy host, it has failed to measure one. That check must report
// unavailable instead of grading; nothing downstream can recover the difference.
func TestNaNGradesOKBecauseV1DoesAndThatIsWhyUnavailableExists(t *testing.T) {
	for _, id := range Defaults().IDs() {
		rule, _ := Defaults().Get(id)
		if got := rule.Grade(math.NaN()); got != OK {
			t.Errorf("%s.Grade(NaN) = %s, want ok to match v1", id, got)
		}
		if _, fired := rule.Check(math.NaN(), ""); fired {
			t.Errorf("%s tripped on NaN", id)
		}
	}
}

// The load rule grades a ratio, and v1 computes that ratio as
// load.one / max(cpu_count, 1). The guard is not in this package -- a rule takes
// the number it is given -- so this records where it has to live instead: a
// collector that divides by a zero cpu_count produces +Inf and grades crit on an
// idle host. runtime.NumCPU never returns zero, but the value v1 grades came
// from a parsed /proc read, and a parse that found nothing returns zero.
func TestLoadRuleGradesARatioAndDoesNotComputeIt(t *testing.T) {
	rule, _ := Defaults().Get(LoadPerCPU)
	// What v1 produces for a 4-core box at load 4 vs a 2-core box at load 4.
	if got := rule.Grade(4.0 / 4); got != Warn {
		t.Errorf("load 4 on 4 cpus = %s, want warn", got)
	}
	if got := rule.Grade(4.0 / 2); got != Crit {
		t.Errorf("load 4 on 2 cpus = %s, want crit", got)
	}
	// And the shape the missing guard would produce.
	if got := rule.Grade(math.Inf(1)); got != Crit {
		t.Errorf("an unguarded divide gives +Inf, which grades %s", got)
	}
}

func TestSeverityStringIsTheWireSpelling(t *testing.T) {
	// These strings are in the frozen public enum; a prettier one is a breaking
	// change to every consumer pinning on `status`.
	for _, tt := range []struct {
		s    Severity
		want string
	}{{OK, "ok"}, {Warn, "warn"}, {Crit, "crit"}} {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("Severity(%d) = %q, want %q", int(tt.s), got, tt.want)
		}
	}
	// An out-of-range value must be legible rather than empty: a status of "" in
	// a report reads as a missing field rather than as a bug here.
	if got := Severity(9).String(); !strings.Contains(got, "9") {
		t.Errorf("Severity(9) = %q, want something naming the value", got)
	}
}

func TestWorseTakesTheWorst(t *testing.T) {
	tests := []struct{ a, b, want Severity }{
		{OK, OK, OK},
		{OK, Warn, Warn},
		{Warn, OK, Warn},
		{Warn, Crit, Crit},
		{Crit, Warn, Crit},
		{Crit, Crit, Crit},
		{OK, Crit, Crit},
	}
	for _, tt := range tests {
		if got := Worse(tt.a, tt.b); got != tt.want {
			t.Errorf("Worse(%s, %s) = %s, want %s", tt.a, tt.b, got, tt.want)
		}
	}
}

// A trip reports the bound it crossed, not just the value. An alert that says
// "82" and nothing else sends the reader to find the configuration.
func TestCheckReportsTheBoundThatFired(t *testing.T) {
	rule, _ := Defaults().Get(DiskUsedPct)

	if _, fired := rule.Check(84.9, "/"); fired {
		t.Error("84.9% tripped the 85% rule")
	}

	warn, fired := rule.Check(90, "/var")
	if !fired {
		t.Fatal("90% did not trip the 85% rule")
	}
	if warn.Bound != 85 || warn.Status != "warn" || warn.Value != 90 {
		t.Errorf("warn trip = %+v, want bound 85 and status warn", warn)
	}
	if warn.Subject != "/var" || warn.Unit != "%" || warn.Rule != DiskUsedPct {
		t.Errorf("trip lost its labelling: %+v", warn)
	}

	// A critical value reports the critical bound, not the warn bound it also
	// crossed -- otherwise a page says 85 while the host is at 99.
	crit, _ := rule.Check(99, "/")
	if crit.Bound != 95 || crit.Status != "crit" {
		t.Errorf("crit trip = %+v, want bound 95 and status crit", crit)
	}
}

func TestValidateRejectsOnlyUnreachableLadders(t *testing.T) {
	tests := []struct {
		name string
		rule Rule
		want string
	}{
		{"ordinary", Rule{ID: "a", Warn: 80, Crit: 95}, ""},
		{"equal bounds skip the warn tier but are reachable", Rule{ID: "a", Warn: 95, Crit: 95}, ""},
		// Unusual is not invalid: a fleet may genuinely want to warn at 50%.
		{"a low bound is a policy, not an error", Rule{ID: "a", Warn: 1, Crit: 2}, ""},
		{"negative bounds are a policy too", Rule{ID: "a", Warn: -10, Crit: 0}, ""},
		{"no id", Rule{Warn: 1, Crit: 2}, "no id"},
		{"inverted", Rule{ID: "a", Warn: 95, Crit: 80}, "warn band is empty"},
		{"NaN warn", Rule{ID: "a", Warn: math.NaN(), Crit: 95}, "NaN"},
		{"NaN crit", Rule{ID: "a", Warn: 80, Crit: math.NaN()}, "NaN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.rule.Validate()
			switch {
			case tt.want == "" && err != nil:
				t.Fatalf("Validate = %v, want nil", err)
			case tt.want != "" && (err == nil || !strings.Contains(err.Error(), tt.want)):
				t.Fatalf("Validate = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
	// Every shipped default has to survive its own validator.
	for _, id := range Defaults().IDs() {
		rule, _ := Defaults().Get(id)
		if err := rule.Validate(); err != nil {
			t.Errorf("shipped default %s: %v", id, err)
		}
	}
}

func TestMergeAppliesOverridesAndWarnsInsteadOfFailing(t *testing.T) {
	set, warnings := Merge(map[string]Rule{
		MemUsedPct: {Warn: 60, Crit: 75},
	})
	got, _ := set.Get(MemUsedPct)
	if got.Warn != 60 || got.Crit != 75 {
		t.Errorf("override not applied: %+v", got)
	}
	// The label travels with the rule, not with the override: an operator writing
	// two numbers cannot relabel a percentage or rename the rule consumers pin on.
	if got.Unit != "%" || got.ID != MemUsedPct || got.Description == "" {
		t.Errorf("override overwrote the rule's identity: %+v", got)
	}
	if len(warnings) != 0 {
		t.Errorf("a good override warned: %v", warnings)
	}
	// Untouched rules keep the defaults.
	if disk, _ := set.Get(DiskUsedPct); disk.Warn != 85 {
		t.Errorf("an unrelated rule changed: %+v", disk)
	}
}

// A bad override must degrade to the default rather than take the check offline
// or install an unreachable ladder. This is the whole contract of the config
// layer, stated once here because thresholds are the part of it that decides
// whether a host pages anyone.
func TestMergeKeepsTheDefaultWhenAnOverrideIsUnusable(t *testing.T) {
	set, warnings := Merge(map[string]Rule{
		DiskUsedPct:    {Warn: 95, Crit: 80}, // inverted
		"disk.usedpct": {Warn: 10, Crit: 20}, // a typo, not a rule
	})

	disk, _ := set.Get(DiskUsedPct)
	if disk.Warn != 85 || disk.Crit != 95 {
		t.Errorf("an inverted override was applied: %+v", disk)
	}
	if _, ok := set.Get("disk.usedpct"); ok {
		t.Error("a typo was installed as a rule, where it would never fire and would look configured")
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %v, want one per unusable override", warnings)
	}
	// The operator has to be able to find the line they wrote.
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"disk.usedpct", DiskUsedPct, "keeping the default"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not mention %q:\n%s", want, joined)
		}
	}
	// Sorted, because the report is asserted byte-stable across consecutive runs
	// and Go's map order is not.
	if warnings[0] > warnings[1] {
		t.Errorf("warnings are not in a stable order: %v", warnings)
	}
}

// Get does not fall back, and this says why: a zero Rule has Warn and Crit at 0,
// so grading against one marks every non-negative value critical. A caller that
// gets false has to decide what a missing rule means rather than be handed a
// grader that pages on an idle host.
func TestGetDoesNotInventARule(t *testing.T) {
	if _, ok := Defaults().Get("nope"); ok {
		t.Fatal("Get invented a rule")
	}
	var zero Rule
	if got := zero.Grade(0); got != Crit {
		t.Fatalf("a zero Rule grades 0 as %s; the danger this guards is gone, so revisit Get", got)
	}
	// Set.Grade is the forgiving one, and grades ok rather than crit.
	if got := Defaults().Grade("nope", 1e9); got != OK {
		t.Errorf("Set.Grade on an unknown rule = %s, want ok", got)
	}
}

func TestIDsAreSortedAndComplete(t *testing.T) {
	ids := Defaults().IDs()
	want := []string{
		LoadPerCPU, DiskInodePct, DiskUsedPct, MemUsedPct,
		ServicesFailed, ThermalTempC, UpdatesSecure,
	}
	if len(ids) != len(want) {
		t.Fatalf("Defaults has %d rules (%v), want %d", len(ids), ids, len(want))
	}
	for i, id := range want {
		if ids[i] != id {
			t.Errorf("IDs()[%d] = %q, want %q (sorted)", i, ids[i], id)
		}
	}
	// Each rule's key and its own ID must agree, or a trip names a rule the
	// operator cannot find in thresholds.yml.
	for key, rule := range Defaults() {
		if rule.ID != key {
			t.Errorf("rule under key %q calls itself %q", key, rule.ID)
		}
		if rule.Unit == "" || rule.Description == "" {
			t.Errorf("%s ships without a unit or description; doctor prints both", key)
		}
	}
}

// Merge returns a fresh set each time, so an override in one process-wide caller
// cannot reach into another's policy.
func TestMergeDoesNotMutateTheDefaults(t *testing.T) {
	if _, _ = Merge(map[string]Rule{MemUsedPct: {Warn: 1, Crit: 2}}); true {
		if mem, _ := Defaults().Get(MemUsedPct); mem.Warn != 80 || mem.Crit != 95 {
			t.Fatalf("Defaults() was mutated by a Merge: %+v", mem)
		}
	}
	a := Defaults()
	a[MemUsedPct] = Rule{ID: MemUsedPct, Warn: 0, Crit: 0}
	if b, _ := Defaults().Get(MemUsedPct); b.Warn != 80 {
		t.Fatal("Defaults() hands out a shared map")
	}
}
