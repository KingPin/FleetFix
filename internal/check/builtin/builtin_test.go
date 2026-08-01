package builtin

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/disk"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// TestRegistryAcceptsEverythingThisBuildShips is the reason MustRegister is the
// right call in a startup path. A duplicated id, a spec missing a title, an id
// that was retired -- each is a defect in builtin.go that this run catches, and
// the alternative to catching it here is a release where a check is silently
// absent from every host's report.
func TestRegistryAcceptsEverythingThisBuildShips(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("this build's checks do not form a valid registry: %v", r)
		}
	}()

	reg := Registry(Deps{})

	if got, want := reg.Len(), len(Checks(Deps{})); got != want {
		t.Errorf("registry holds %d checks, want the %d this build ships", got, want)
	}
	if reg.Len() == 0 {
		t.Fatal("the registry is empty; every front door would report that it found nothing to do")
	}
}

// The ids are the public surface -- they appear in checks[], in --check
// selectors, and in whatever Ansible an operator wrote against them. Naming them
// here rather than counting means dropping one from the build is a failure with
// the missing name in it.
func TestTheDiskDomainIsRegistered(t *testing.T) {
	reg := Registry(Deps{})

	for _, id := range []check.ID{disk.UsageID, disk.InodesID} {
		found := false
		for _, spec := range reg.Specs() {
			if spec.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not registered in this build", id)
		}
	}
}

// A nil Deps.Run is the documented "give me the real one", and it has to work:
// `check --list` and doctor both want to know what this build can check without
// having any interest in running a subprocess.
func TestANilRunnerMeansTheRealOne(t *testing.T) {
	if got := (Deps{}).runner(); got == nil {
		t.Fatal("Deps{}.runner() is nil; the collectors would panic on their first call")
	}

	checks := Checks(Deps{})
	if len(checks) == 0 {
		t.Fatal("Checks(Deps{}) returned nothing")
	}
	for _, c := range checks {
		if err := c.Spec().Validate(); err != nil {
			t.Errorf("%s: %v", c.Spec().ID, err)
		}
	}
}

// TestTheStagedRunnerReachesTheChecks is the property the Deps struct exists for.
// A collector that closed over cmdrun.New() itself would pass every test above and
// still shell out to the real df in a unit test.
func TestTheStagedRunnerReachesTheChecks(t *testing.T) {
	fake := cmdrun.NewFake()
	reg := Registry(Deps{Run: fake})

	selected, err := reg.Select([]string{string(disk.UsageID)}, nil)
	if err != nil {
		t.Fatalf("selecting %s failed: %v", disk.UsageID, err)
	}
	if len(selected) != 1 {
		t.Fatalf("selected %d checks, want 1", len(selected))
	}

	// The staged runner answers nothing, so the check reports a failure rather
	// than a reading. What is under test is which runner it reached, not the
	// verdict -- internal/check/builtin/disk owns the verdict.
	selected[0].Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	})
	if len(fake.Calls()) == 0 {
		t.Fatal("the staged runner was never called; the checks hold their own cmdrun.New()")
	}
	if calls := strings.Join(fake.Calls(), " "); !strings.Contains(calls, "df") {
		t.Errorf("calls = %q, want the disk collector's df among them", calls)
	}
}
