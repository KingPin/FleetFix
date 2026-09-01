package config

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// The whole point of the layer, end to end: a fleet-wide file tightens a bound for
// every host, one host's operator loosens it back, and the grading follows.
func TestThresholdsMergeAcrossLayers(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, ThresholdsFile,
		"disk.used_pct:\n  warn: 70\n  crit: 80\nmem.used_pct:\n  warn: 60\n")
	writeYAML(t, p.UserDir, ThresholdsFile,
		"disk.used_pct:\n  warn: 75\n")

	set, loaded := p.Thresholds()
	if len(loaded.Warnings) != 0 {
		t.Fatalf("a valid pair of files warned: %v", loaded.Warnings)
	}

	disk, _ := set.Get(threshold.DiskUsedPct)
	// warn from the user's file, crit from /etc: the mapping merged key by key, so
	// the personal file did not have to restate the fleet's crit to change warn.
	if disk.Warn != 75 || disk.Crit != 80 {
		t.Errorf("disk = warn %g crit %g, want warn 75 (user) crit 80 (fleet)", disk.Warn, disk.Crit)
	}
	// A rule only /etc mentions still applies.
	if mem, _ := set.Get(threshold.MemUsedPct); mem.Warn != 60 || mem.Crit != 95 {
		t.Errorf("mem = warn %g crit %g, want warn 60 (fleet) crit 95 (shipped)", mem.Warn, mem.Crit)
	}
	// A rule neither file mentions is untouched.
	if load, _ := set.Get(threshold.LoadPerCPU); load.Warn != 1.0 || load.Crit != 2.0 {
		t.Errorf("an unmentioned rule changed: %+v", load)
	}

	if got := len(loaded.Effective()); got != 2 {
		t.Errorf("Effective() named %d files, want both layers", got)
	}
}

// A ladder can be inverted by two files that are each fine on their own, which is
// the failure mode the layering introduces and the reason Validate runs after the
// merge rather than on each file. The operator sees their own file, sees nothing
// wrong with it, and needs the warning to name the rule so they think to look at
// /etc as well.
func TestALadderInvertedAcrossTwoLayersIsCaught(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.SystemDir, ThresholdsFile, "disk.used_pct:\n  crit: 80\n")
	writeYAML(t, p.UserDir, ThresholdsFile, "disk.used_pct:\n  warn: 90\n")

	set, loaded := p.Thresholds()
	if len(loaded.Warnings) != 1 {
		t.Fatalf("warnings = %v, want one", loaded.Warnings)
	}
	if !strings.Contains(loaded.Warnings[0], threshold.DiskUsedPct) {
		t.Errorf("the warning does not name the rule: %s", loaded.Warnings[0])
	}
	// Both bounds go back to the shipped pair. Keeping half of an inverted override
	// would install a ladder neither file asked for.
	if disk, _ := set.Get(threshold.DiskUsedPct); disk.Warn != 85 || disk.Crit != 95 {
		t.Errorf("disk = %+v, want the shipped bounds", disk)
	}
}

// Most hosts never write the file, and that has to be the quiet path: the shipped
// policy, no warnings, and a search path doctor can still print.
func TestThresholdsWithNoFileIsTheShippedPolicy(t *testing.T) {
	p := testPaths(t)
	set, loaded := p.Thresholds()
	if len(loaded.Warnings) != 0 {
		t.Errorf("an absent file warned: %v", loaded.Warnings)
	}
	if len(loaded.Effective()) != 0 {
		t.Errorf("Effective() = %v, want nothing", loaded.Effective())
	}
	for id, want := range threshold.Defaults() {
		if set[id] != want {
			t.Errorf("%s = %+v, want the shipped %+v", id, set[id], want)
		}
	}
	// doctor prints the candidates even when none of them exist, so the operator
	// can see the path they meant to write is not the path being read.
	if got := len(loaded.DescribeLayers()); got != 2 {
		t.Errorf("DescribeLayers() = %v, want both candidates", loaded.DescribeLayers())
	}
}

// Every way the file can be wrong, in one host's configuration, all reported and
// all survived. A fleet tool that stops grading over a typo is worse than one
// grading on defaults, so the policy that comes back is usable in every case.
func TestThresholdsReportEveryProblemAndKeepGrading(t *testing.T) {
	p := testPaths(t)
	// Unparseable: a tab where YAML forbids one.
	writeYAML(t, p.SystemDir, ThresholdsFile, "disk.used_pct:\n\twarn: 70\n")
	writeYAML(t, p.UserDir, ThresholdsFile, strings.Join([]string{
		`disk.usedpct:`, `  warn: 10`, // a rule that does not exist
		`mem.used_pct:`, `  warn: "ninety"`, // a bound that is not a number
		`thermal.temp_c:`, `  warn: 90`, `  crit: 60`, // inverted
		`services.failed:`, `  warn: 2`, // and one that is simply fine
	}, "\n")+"\n")

	set, loaded := p.Thresholds()

	joined := strings.Join(loaded.Warnings, "\n")
	for _, want := range []string{
		"ignoring this file", // the unparseable fleet layer
		"disk.usedpct",       // the invented rule
		"mem.used_pct",       // the string bound
		"thermal.temp_c",     // the inverted ladder
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not mention %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, threshold.ServicesFailed) {
		t.Errorf("the one good override warned:\n%s", joined)
	}

	// The good override took, and everything else is the shipped value rather than
	// a zero, an inverted ladder, or a missing rule.
	if svc, _ := set.Get(threshold.ServicesFailed); svc.Warn != 2 || svc.Crit != 3 {
		t.Errorf("services = %+v, want warn 2 crit 3", svc)
	}
	if disk, _ := set.Get(threshold.DiskUsedPct); disk.Warn != 85 || disk.Crit != 95 {
		t.Errorf("disk = %+v, want the shipped bounds", disk)
	}
	if mem, _ := set.Get(threshold.MemUsedPct); mem.Warn != 80 {
		t.Errorf("mem = %+v, want the shipped bounds", mem)
	}
	if tmp, _ := set.Get(threshold.ThermalTempC); tmp.Warn != 70 || tmp.Crit != 85 {
		t.Errorf("thermal = %+v, want the shipped bounds", tmp)
	}
	for _, id := range threshold.Defaults().IDs() {
		if _, ok := set.Get(id); !ok {
			t.Errorf("%s went missing from the policy", id)
		}
	}

	// The unparseable layer is named as unparseable rather than as absent -- an
	// operator whose file is being skipped must not read "(absent)" and go looking
	// for a path problem.
	layers := strings.Join(loaded.DescribeLayers(), "\n")
	if !strings.Contains(layers, "unparseable") {
		t.Errorf("DescribeLayers hides the broken file:\n%s", layers)
	}
}

// The report is asserted byte-stable across consecutive runs, so the warnings the
// same configuration produces must not depend on Go's map order.
func TestThresholdWarningsAreStableAcrossRuns(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, ThresholdsFile, strings.Join([]string{
		`zzz.rule:`, `  warn: 1`,
		`aaa.rule:`, `  warn: 1`,
		`mem.used_pct:`, `  warn: "no"`, `  crit: "no"`,
	}, "\n")+"\n")

	_, first := p.Thresholds()
	if len(first.Warnings) != 4 {
		t.Fatalf("warnings = %v, want one per problem", first.Warnings)
	}
	want := strings.Join(first.Warnings, "\n")
	for range 20 {
		_, again := p.Thresholds()
		if strings.Join(again.Warnings, "\n") != want {
			t.Fatalf("warnings vary between runs:\n%s\n---\n%s", want, strings.Join(again.Warnings, "\n"))
		}
	}
}

// A top level that is not a mapping contributes nothing, the same as every other
// loader in this package: v1's contract is that a call site always gets a mapping.
func TestThresholdsIgnoreANonMappingFile(t *testing.T) {
	p := testPaths(t)
	writeYAML(t, p.UserDir, ThresholdsFile, "- disk.used_pct\n- mem.used_pct\n")
	set, loaded := p.Thresholds()
	if len(loaded.Warnings) != 0 {
		t.Errorf("warnings = %v, want none", loaded.Warnings)
	}
	if disk, _ := set.Get(threshold.DiskUsedPct); disk.Warn != 85 {
		t.Errorf("a list changed the policy: %+v", disk)
	}
}
