package services

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// The fixtures are the M0 corpus, the same bytes internal/core/services' table
// tests and the Python oracle read. Nothing here restates what the parsers
// produce -- that is already pinned against Python -- so these tests are about
// the layer the port adds: which status, which trips, which metrics, and what the
// summary says.

func byID(t *testing.T, id check.ID, run cmdrun.Runner) check.Check {
	t.Helper()
	for _, c := range Checks(run) {
		if c.Spec().ID == id {
			return c
		}
	}
	t.Fatalf("no check with id %s", id)
	return nil
}

// run drives one check and returns the normalised Result alongside what it
// emitted. Normalize because the runner applies it to everything on its way out,
// and asserting on a Result the report would never carry tests a shape nothing
// ships.
func run(t *testing.T, id check.ID, r cmdrun.Runner) (check.Result, []check.Event) {
	t.Helper()
	return runGraded(t, id, r, threshold.Defaults())
}

func runGraded(t *testing.T, id check.ID, r cmdrun.Runner, set threshold.Set) (check.Result, []check.Event) {
	t.Helper()
	var steps []check.Event
	res := byID(t, id, r).Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.EmitterFunc(func(e check.Event) { steps = append(steps, e) }),
		Thresholds: set,
	})
	return res.Normalize(), steps
}

func metricNamed(t *testing.T, res check.Result, name string) check.Metric {
	t.Helper()
	for _, m := range res.Metrics {
		if m.Name == name {
			return m
		}
	}
	t.Fatalf("no %s metric among %v", name, res.Metrics)
	return check.Metric{}
}

func texts(steps []check.Event) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Text
	}
	return out
}

// The domain's shape, asserted once.
func TestTheDomainShipsTwoChecks(t *testing.T) {
	checks := Checks(cmdrun.NewFake())

	if len(checks) != 2 {
		t.Fatalf("the domain has %d checks, want 2", len(checks))
	}
	seen := map[check.ID]bool{}
	for _, c := range checks {
		spec := c.Spec()
		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", spec.ID, err)
		}
		if spec.Domain != "services" {
			t.Errorf("%s is in domain %q", spec.ID, spec.Domain)
		}
		if !spec.InDefault {
			t.Errorf("%s is not in the default run", spec.ID)
		}
		if seen[spec.ID] {
			t.Errorf("%s is registered twice", spec.ID)
		}
		seen[spec.ID] = true
	}
	for _, id := range []check.ID{FailedID, BootID} {
		if !seen[id] {
			t.Errorf("%s is missing from the domain", id)
		}
	}
}

// Both checks declare what they shell out to, so the runner reports a host
// without systemd once rather than each check discovering it separately.
func TestBothChecksDeclareTheirBinary(t *testing.T) {
	for _, tc := range []struct {
		id  check.ID
		bin string
	}{
		{FailedID, systemctlBin},
		{BootID, analyzeBin},
	} {
		spec := byID(t, tc.id, cmdrun.NewFake()).Spec()
		if len(spec.NeedsBins) != 1 || spec.NeedsBins[0] != tc.bin {
			t.Errorf("%s needs %v, want [%s]", tc.id, spec.NeedsBins, tc.bin)
		}
	}
}

// And each says so itself when nothing gated it -- a Runner with no Looker, which
// is what a test and a front door that skipped the gate both have. unavailable
// rather than a fault: a host without systemd is an Alpine container or a BSD,
// not a broken machine.
func TestAMissingBinaryIsUnavailableRatherThanAFault(t *testing.T) {
	for _, tc := range []struct {
		id   check.ID
		bin  string
		args []string
	}{
		{FailedID, systemctlBin, listArgs},
		{BootID, analyzeBin, blameArgs},
	} {
		fake := cmdrun.NewFake()
		fake.Missing(tc.bin, tc.args...)

		res, _ := run(t, tc.id, fake)
		if res.Status != check.StatusUnavailable {
			t.Errorf("%s = %s, want unavailable", tc.id, res.Status)
		}
		if !strings.Contains(res.Summary, tc.bin) {
			t.Errorf("%s summary = %q, want the binary named", tc.id, res.Summary)
		}
	}
}

func TestPlural(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{plural(1, "failed unit"), "1 failed unit"},
		{plural(0, "failed unit"), "0 failed units"},
		{plural(3, "failed unit"), "3 failed units"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}

func TestFirstLineFallsBackWhenThereIsNothingToQuote(t *testing.T) {
	if got := firstLine("  \n\n ", "exited 2"); got != "exited 2" {
		t.Errorf("firstLine(whitespace) = %q, want the fallback", got)
	}
	if got := firstLine("\n  real complaint  \nsecond", ""); got != "real complaint" {
		t.Errorf("firstLine = %q, want the first non-blank line trimmed", got)
	}
}
