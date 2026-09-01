package services

import (
	"errors"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coreservices "github.com/KingPin/FleetFix/v2/internal/core/services"
	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// staged answers the listing with the named fixture and nothing else, so a test
// that does not care about owners still exercises the path that asks for them.
func staged(t *testing.T, listing string) *cmdrun.Fake {
	t.Helper()
	fake := cmdrun.NewFake()
	fake.Stdout(fixture.Text(t, listing), systemctlBin, listArgs...)
	return fake
}

// withOwners stages the second call too, for the units the listing names.
func withOwners(t *testing.T, listing, users string, units ...string) *cmdrun.Fake {
	t.Helper()
	fake := staged(t, listing)
	fake.Stdout(fixture.Text(t, users), systemctlBin, append(append([]string{}, showArgs...), units...)...)
	return fake
}

func TestNoFailedUnitsIsOK(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Stdout("", systemctlBin, listArgs...)

	res, steps := run(t, FailedID, fake)

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	if res.Summary != "no failed units" {
		t.Errorf("summary = %q", res.Summary)
	}
	if got := metricNamed(t, res, FailedMetric).Value; got != 0 {
		t.Errorf("%s = %v, want 0 -- a healthy host still reports the number", FailedMetric, got)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v; a healthy host has nothing to narrate", texts(steps))
	}
	// And the second call is not made, because there is nothing to ask about.
	if len(fake.Calls()) != 1 {
		t.Errorf("calls = %v, want only the listing", fake.Calls())
	}
}

// v1's bounds: warn at one failed unit, crit at three.
func TestFailedUnitsAreGradedByCount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		listing string
		units   []string
		status  check.Status
	}{
		{"two units", "systemctl/failed_two_units.txt", []string{"myapp.service", "other.service"}, check.StatusWarn},
		{
			"three units", "systemctl/failed_three_units.txt",
			[]string{"alpha.service", "beta.service", "gamma.service"},
			check.StatusCrit,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := run(t, FailedID, staged(t, tc.listing))

			if res.Status != tc.status {
				t.Errorf("status = %s, want %s (summary %q)", res.Status, tc.status, res.Summary)
			}
			if len(res.Trips) != 1 || res.Trips[0].Subject != "failed units" {
				t.Errorf("trips = %+v", res.Trips)
			}
			if got := metricNamed(t, res, FailedMetric).Value; got != float64(len(tc.units)) {
				t.Errorf("%s = %v, want %d", FailedMetric, got, len(tc.units))
			}
		})
	}
	if FailedMetric != threshold.ServicesFailed {
		t.Errorf("the metric is %q and the rule is %q", FailedMetric, threshold.ServicesFailed)
	}
}

// The summary names every unit, because the count is what got graded and the
// names are what somebody does something about.
func TestTheSummaryNamesEveryFailedUnit(t *testing.T) {
	res, _ := run(t, FailedID, staged(t, "systemctl/failed_three_units.txt"))

	want := "3 failed units: alpha.service, beta.service, gamma.service"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

// One step per unit, carrying systemd's own description and -- when the second
// call answered -- who the unit runs as.
func TestEachFailedUnitGetsAStepNamingItsOwner(t *testing.T) {
	fake := withOwners(t, "systemctl/failed_two_units.txt", "systemctl/show_user_two_blocks.txt",
		"myapp.service", "other.service")

	res, steps := run(t, FailedID, fake)

	if len(steps) != 2 {
		t.Fatalf("steps = %v, want one per unit", texts(steps))
	}
	for _, s := range steps {
		if s.Status != check.StatusWarn {
			t.Errorf("step %q has status %s", s.Text, s.Status)
		}
	}
	if want := "myapp.service (My Application Service) is failed, running as root"; steps[0].Text != want {
		t.Errorf("step = %q, want %q", steps[0].Text, want)
	}
	// data[] carries the owner too, so a consumer does not have to parse the step.
	rows, ok := res.Data.([]Unit)
	if !ok {
		t.Fatalf("data is %T, want []Unit", res.Data)
	}
	if len(rows) != 2 || rows[1].Owner == "" {
		t.Errorf("rows = %+v, want both owners filled in", rows)
	}
}

// A unit with no explicit User= runs as root, which systemd's own default makes
// the answer rather than a guess -- and it is the difference between a failed
// unit being one operator's problem and being the host's.
func TestAUnitWithNoUserRunsAsRoot(t *testing.T) {
	fake := withOwners(t, "systemctl/failed_three_units.txt", "systemctl/show_user_three_blocks.txt",
		"alpha.service", "beta.service", "gamma.service")

	res, _ := run(t, FailedID, fake)

	rows, _ := res.Data.([]Unit)
	for i, want := range []string{"root", "appuser", "root"} {
		if rows[i].Owner != want {
			t.Errorf("%s runs as %q, want %q", rows[i].Name, rows[i].Owner, want)
		}
	}
}

// The owners are an annotation, not a gate. v1 used the same call to filter and
// returned an empty list when it did not line up, so a mismatch there meant
// "nothing has failed" -- here it costs the owners and keeps the units.
func TestAnUnansweredOwnerLookupKeepsTheUnits(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stage func(*cmdrun.Fake)
		want  string
	}{
		{
			"the call failed",
			func(f *cmdrun.Fake) {
				f.Fail(errors.New("context deadline exceeded"), systemctlBin,
					append(append([]string{}, showArgs...), "myapp.service", "other.service")...)
			},
			"context deadline exceeded",
		},
		{
			"a non-zero exit",
			func(f *cmdrun.Fake) {
				f.Exit(1, "", "Failed to get properties: Unit not loaded", systemctlBin,
					append(append([]string{}, showArgs...), "myapp.service", "other.service")...)
			},
			"Unit not loaded",
		},
		{
			"the wrong number of blocks",
			func(f *cmdrun.Fake) {
				f.Stdout("User=root\n", systemctlBin,
					append(append([]string{}, showArgs...), "myapp.service", "other.service")...)
			},
			"described 1 of 2 units",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := staged(t, "systemctl/failed_two_units.txt")
			tc.stage(fake)

			res, steps := run(t, FailedID, fake)

			if res.Status != check.StatusWarn {
				t.Errorf("status = %s, want warn -- the count is still graded", res.Status)
			}
			if !strings.Contains(strings.Join(texts(steps), "\n"), tc.want) {
				t.Errorf("steps = %v, want one saying why the owners are missing", texts(steps))
			}
			rows, _ := res.Data.([]Unit)
			if len(rows) != 2 {
				t.Fatalf("rows = %+v, want both units kept", rows)
			}
			// Empty rather than "root". An owner nobody looked up is not root, and
			// rendering it as root would name the wrong person.
			if rows[0].Owner != "" {
				t.Errorf("owner = %q, want empty when the lookup did not answer", rows[0].Owner)
			}
			if strings.Contains(texts(steps)[len(steps)-1], "running as") {
				t.Errorf("a step claimed an owner: %q", texts(steps)[len(steps)-1])
			}
		})
	}
}

// systemctl is installed and refused. v1 returned an empty list here and drew a
// pane saying nothing had failed, which is the permanently-green failure this
// layer exists to prevent.
func TestARefusedListingIsAnErrorRatherThanNoFailures(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Exit(1, "", "Failed to connect to bus: No such file or directory", systemctlBin, listArgs...)

	res, _ := run(t, FailedID, fake)

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Error, "Failed to connect to bus") {
		t.Errorf("error = %q, want systemd's own words", res.Error)
	}
}

func TestAListingThatDidNotRunIsAnError(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Fail(errors.New("context deadline exceeded"), systemctlBin, listArgs...)

	res, _ := run(t, FailedID, fake)

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Error, "deadline") {
		t.Errorf("error = %q", res.Error)
	}
}

// A policy with no rule for what this check measures is an error, not a green
// host -- and the refusal happens before the host is touched, since the answer
// cannot depend on it.
func TestAMissingRuleIsAnErrorReportedWithoutRunningAnything(t *testing.T) {
	fake := cmdrun.NewFake()

	res, _ := runGraded(t, FailedID, fake, threshold.Set{})

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Error, threshold.ServicesFailed) {
		t.Errorf("error = %q, want the missing rule named", res.Error)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %v; the host was read to answer a question about policy", fake.Calls())
	}
}

// A unit with no description still reads as a sentence.
func TestAUnitWithNoDescriptionStillReadsAsALine(t *testing.T) {
	got := describe(Unit{
		FailedUnit: coreservices.FailedUnit{Name: "orphan.service", Sub: "failed"},
		Owner:      "root",
	})

	if want := "orphan.service is failed, running as root"; got != want {
		t.Errorf("describe = %q, want %q", got, want)
	}
}
