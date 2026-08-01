package threshold

import (
	"math"
	"math/big"
	"strings"
	"testing"
)

// The end-to-end claim the whole file exists for: an operator writes two numbers,
// the host grades by them, and everything they did not write is unchanged.
func TestAFileAnOperatorWouldWriteChangesTheGrading(t *testing.T) {
	// The shape of thresholds.yml as internal/config decodes it: !!int is int64,
	// !!float is float64, and both appear in a file anyone would actually type.
	body := map[string]any{
		DiskUsedPct: map[string]any{"warn": int64(75), "crit": int64(90)},
		LoadPerCPU:  map[string]any{"warn": 0.75},
	}
	overrides, warnings := ParseOverrides(body)
	if len(warnings) != 0 {
		t.Fatalf("a valid file warned: %v", warnings)
	}
	set, warnings := Merge(overrides)
	if len(warnings) != 0 {
		t.Fatalf("a valid file warned on merge: %v", warnings)
	}

	// 80% is ok on the shipped bound of 85 and a warning on the operator's 75.
	if got := set.Grade(DiskUsedPct, 80); got != Warn {
		t.Errorf("disk 80%% graded %s, want warn -- the override did not take", got)
	}
	if got := set.Grade(DiskUsedPct, 92); got != Crit {
		t.Errorf("disk 92%% graded %s, want crit", got)
	}
	// crit was not written, so it stays where v1 put it rather than becoming zero.
	load, _ := set.Get(LoadPerCPU)
	if load.Warn != 0.75 || load.Crit != 2.0 {
		t.Errorf("partial override = warn %g crit %g, want warn 0.75 crit 2 (the shipped crit)", load.Warn, load.Crit)
	}
	if mem, _ := set.Get(MemUsedPct); mem.Warn != 80 || mem.Crit != 95 {
		t.Errorf("a rule the file never mentioned changed: %+v", mem)
	}
}

// The bug this guards is the reason Override holds pointers. A struct of plain
// floats decoded from a file that sets only warn carries crit=0, and every rule
// whose crit went to zero marks a healthy host critical -- so the operator who
// tightened one warning silently turned the whole fleet red.
func TestAnAbsentBoundIsNotZero(t *testing.T) {
	overrides, _ := ParseOverrides(map[string]any{
		MemUsedPct: map[string]any{"warn": int64(90)},
	})
	if overrides[MemUsedPct].Crit != nil {
		t.Fatal("a bound the file did not mention was parsed as written")
	}
	set, _ := Merge(overrides)
	if got := set.Grade(MemUsedPct, 5); got != OK {
		t.Errorf("an idle host graded %s after a warn-only override", got)
	}
}

func TestParseOverridesReadsEveryNumericTypeTheDecoderProduces(t *testing.T) {
	huge, _ := new(big.Int).SetString("99999999999999999999", 10)
	overrides, warnings := ParseOverrides(map[string]any{
		"a": map[string]any{"warn": int64(1)},
		"b": map[string]any{"warn": 1.5},
		"c": map[string]any{"warn": huge},
		"d": map[string]any{"warn": math.Inf(1)},
	})
	if len(warnings) != 0 {
		t.Fatalf("a numeric value warned: %v", warnings)
	}
	for id, want := range map[string]float64{"a": 1, "b": 1.5, "c": 1e20, "d": math.Inf(1)} {
		got := overrides[id].Warn
		if got == nil {
			t.Errorf("%s: not parsed", id)
			continue
		}
		if *got != want {
			t.Errorf("%s: warn = %g, want %g", id, *got, want)
		}
	}
}

// Each rejection names the key the operator wrote, because a threshold that was
// silently not applied is a host that then did not page.
func TestParseOverridesRejectsWhatIsNotANumberAndSaysWhere(t *testing.T) {
	for _, tc := range []struct {
		name  string
		body  map[string]any
		wants []string
	}{
		{
			// Quoting is the operator saying they meant a string. Coercing it back
			// would make the one typo that changes a document's meaning invisible.
			name:  "a quoted number",
			body:  map[string]any{DiskUsedPct: map[string]any{"warn": "90"}},
			wants: []string{DiskUsedPct, "warn", "string"},
		},
		{
			name:  "a bool",
			body:  map[string]any{DiskUsedPct: map[string]any{"crit": true}},
			wants: []string{DiskUsedPct, "crit", "bool"},
		},
		{
			// `warn:` with nothing after it. YAML says null; the operator meant to
			// come back to it.
			name:  "an empty value",
			body:  map[string]any{DiskUsedPct: map[string]any{"warn": nil}},
			wants: []string{DiskUsedPct, "warn", "empty"},
		},
		{
			name:  "a bare number where a mapping belongs",
			body:  map[string]any{DiskUsedPct: int64(90)},
			wants: []string{DiskUsedPct, "mapping"},
		},
		{
			name:  "a misspelled bound",
			body:  map[string]any{DiskUsedPct: map[string]any{"warning": int64(90)}},
			wants: []string{DiskUsedPct, "warning"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			overrides, warnings := ParseOverrides(tc.body)
			if len(overrides) != 0 {
				t.Errorf("an unusable value was kept: %+v", overrides)
			}
			if len(warnings) != 1 {
				t.Fatalf("warnings = %v, want exactly one", warnings)
			}
			for _, want := range tc.wants {
				if !strings.Contains(warnings[0], want) {
					t.Errorf("warning does not mention %q: %s", want, warnings[0])
				}
			}
		})
	}
}

// A rule with one good bound and one bad one keeps the good one: refusing the
// whole rule would discard a setting the operator got right.
func TestOneBadBoundDoesNotDiscardTheOther(t *testing.T) {
	overrides, warnings := ParseOverrides(map[string]any{
		DiskUsedPct: map[string]any{"warn": int64(75), "crit": "ninety"},
	})
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one", warnings)
	}
	ov := overrides[DiskUsedPct]
	if ov.Warn == nil || *ov.Warn != 75 {
		t.Errorf("the good bound was discarded: %+v", ov)
	}
	if ov.Crit != nil {
		t.Errorf("the bad bound was kept: %g", *ov.Crit)
	}
}

// .nan parses, so it reaches here as a number and has to be caught by Validate
// rather than by the decoder. Nothing can ever cross a NaN bound, so an operator
// who wrote one has turned that tier off without meaning to.
func TestANaNBoundIsAParsedNumberAndARejectedRule(t *testing.T) {
	overrides, warnings := ParseOverrides(map[string]any{
		MemUsedPct: map[string]any{"warn": math.NaN()},
	})
	if len(warnings) != 0 {
		t.Fatalf("ParseOverrides rejected a number: %v", warnings)
	}
	set, warnings := Merge(overrides)
	if len(warnings) != 1 || !strings.Contains(warnings[0], "NaN") {
		t.Fatalf("Merge warnings = %v, want one naming NaN", warnings)
	}
	if mem, _ := set.Get(MemUsedPct); mem.Warn != 80 {
		t.Errorf("a NaN bound was installed: %+v", mem)
	}
}

// An empty or absent file is the normal case -- most hosts never write one -- so
// it produces the shipped policy and says nothing.
func TestNoFileIsTheShippedPolicy(t *testing.T) {
	for name, body := range map[string]map[string]any{
		"absent": nil,
		"empty":  {},
		// A rule id present with an empty body: the operator started a stanza and
		// wrote no bounds. Nothing to apply, and nothing wrong either.
		"a stanza with no bounds": {DiskUsedPct: map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			overrides, warnings := ParseOverrides(body)
			if len(overrides) != 0 || len(warnings) != 0 {
				t.Fatalf("overrides = %+v, warnings = %v, want neither", overrides, warnings)
			}
			set, warnings := Merge(overrides)
			if len(warnings) != 0 {
				t.Errorf("warnings = %v, want none", warnings)
			}
			for id, want := range Defaults() {
				if set[id] != want {
					t.Errorf("%s = %+v, want the shipped %+v", id, set[id], want)
				}
			}
		})
	}
}

// Two runs over the same file must produce byte-identical warnings; the report is
// asserted byte-stable across consecutive runs and Go's map order is not.
func TestWarningsAreOrderedByWhatTheOperatorWrote(t *testing.T) {
	body := map[string]any{
		"zzz.rule":  map[string]any{"warn": "no"},
		"aaa.rule":  map[string]any{"warn": "no"},
		"mmm.rule":  map[string]any{"warn": "no"},
		DiskUsedPct: map[string]any{"crit": "no", "warn": "no"},
	}
	_, first := ParseOverrides(body)
	if len(first) != 5 {
		t.Fatalf("warnings = %v, want one per bad value", first)
	}
	for i := 1; i < len(first); i++ {
		if first[i-1] > first[i] {
			t.Fatalf("warnings are not sorted: %v", first)
		}
	}
	for range 20 {
		_, again := ParseOverrides(body)
		if strings.Join(again, "\n") != strings.Join(first, "\n") {
			t.Fatalf("warnings vary between runs:\n%v\n%v", first, again)
		}
	}
}

// doctor's listing has to answer "is my file in effect?", which a table of numbers
// alone cannot: an operator who set 85 on a rule that already shipped at 85 and one
// whose file was never read see the same numbers.
func TestDescribeMarksWhatTheOperatorChanged(t *testing.T) {
	overrides, _ := ParseOverrides(map[string]any{
		DiskUsedPct: map[string]any{"warn": int64(75)},
	})
	set, _ := Merge(overrides)
	lines := set.Describe()
	if len(lines) != len(Defaults()) {
		t.Fatalf("Describe printed %d lines for %d rules", len(lines), len(Defaults()))
	}
	var marked []string
	for _, line := range lines {
		if strings.Contains(line, "(overridden)") {
			marked = append(marked, line)
		}
	}
	if len(marked) != 1 || !strings.Contains(marked[0], DiskUsedPct) {
		t.Fatalf("marked %v, want only %s", marked, DiskUsedPct)
	}
	// The unit and description are what make the number readable without the source.
	if !strings.Contains(marked[0], "%") || !strings.Contains(marked[0], "capacity") {
		t.Errorf("the line is not self-explanatory: %s", marked[0])
	}
	// Sorted, and every rule named, so the listing is a file the operator can copy.
	for i, id := range Defaults().IDs() {
		if !strings.HasPrefix(lines[i], id) {
			t.Errorf("line %d = %q, want it to start with %q", i, lines[i], id)
		}
	}
}

// A rule invented in an operator's file must not appear in Describe as though the
// host grades by it -- Merge drops it, and this is the check that Describe agrees.
func TestDescribeShowsOnlyRulesTheHostGradesBy(t *testing.T) {
	overrides, _ := ParseOverrides(map[string]any{
		"disk.usedpct": map[string]any{"warn": int64(1)},
	})
	set, warnings := Merge(overrides)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want one for the typo", warnings)
	}
	for _, line := range set.Describe() {
		if strings.Contains(line, "disk.usedpct") {
			t.Errorf("a rule nothing grades by is listed: %s", line)
		}
	}
}
