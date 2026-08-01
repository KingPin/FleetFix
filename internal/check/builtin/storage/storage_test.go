package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// There is no captured corpus for this domain, and there could not be: what these
// two checks read is a directory tree and a file an operator named, rather than a
// tool's output that could be recorded once. The fixtures are therefore tempdirs,
// which is the seam the package doc argues for, and the tests are about the layer
// the port adds -- which status, which metrics, and what the summary says.

// staged is one file to write: how big, and how long before "now" it was last
// touched.
type staged struct {
	size int
	age  time.Duration
}

// tree writes files under a fresh tempdir, creating parents as needed, and
// returns the root.
//
// The contents are zeroes because only the size is read; the mtime is what the
// cutoff turns on, so every file gets one relative to the test's own clock rather
// than to whenever the suite happened to run.
func tree(t *testing.T, now time.Time, files map[string]staged) string {
	t.Helper()
	root := t.TempDir()
	for name, f := range files {
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", name, err)
		}
		if err := os.WriteFile(full, make([]byte, f.size), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		at := now.Add(-f.age)
		if err := os.Chtimes(full, at, at); err != nil {
			t.Fatalf("chtimes %s: %v", name, err)
		}
	}
	return root
}

// fixedClock is the test's "now". A date rather than time.Now so a failure
// message reads the same on every run.
var fixedClock = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)

func runCheck(t *testing.T, c check.Check, params map[string]string) (check.Result, []check.Event) {
	t.Helper()
	var steps []check.Event
	res := c.Run(t.Context(), check.Input{
		Params:     params,
		Progress:   check.EmitterFunc(func(e check.Event) { steps = append(steps, e) }),
		Thresholds: threshold.Defaults(),
	})
	return res.Normalize(), steps
}

// runStale runs the stale check against the test's fixed clock.
func runStale(t *testing.T, root, days string) (check.Result, []check.Event) {
	t.Helper()
	return runCheck(t, stale{now: func() time.Time { return fixedClock }}, map[string]string{
		RootParam:          root,
		OlderThanDaysParam: days,
	})
}

func runEnv(t *testing.T, path, required string) (check.Result, []check.Event) {
	t.Helper()
	return runCheck(t, env{}, map[string]string{
		PathParam:         path,
		RequiredKeysParam: required,
	})
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

// skipAsRoot bows out of the tests that turn on a refusal. Running the suite as
// root is a legitimate thing to do -- the tier2 marker exists for it -- and root
// is not refused by a mode, so these would assert the opposite of what they mean.
func skipAsRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root is not refused by a file mode, so there is nothing here to observe")
	}
}

func TestTheDomainRegistersBothChecks(t *testing.T) {
	checks := Checks()
	if len(checks) != 2 {
		t.Fatalf("the domain has %d checks, want 2", len(checks))
	}
	want := map[check.ID]bool{StaleID: true, EnvID: true}
	for _, c := range checks {
		spec := c.Spec()
		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", spec.ID, err)
		}
		if !want[spec.ID] {
			t.Errorf("unexpected check %s", spec.ID)
		}
		delete(want, spec.ID)
		// The whole argument of the package doc: neither of these runs without a
		// path, so neither belongs in a bare `fleetfix check`.
		if spec.InDefault {
			t.Errorf("%s is in the default set, but it cannot run without a path", spec.ID)
		}
		if len(spec.Params) == 0 {
			t.Errorf("%s declares no parameters, so the runner cannot skip it for want of one", spec.ID)
		}
	}
	for id := range want {
		t.Errorf("%s is not registered", id)
	}
}

// The two checks must be reachable without a live host underneath them, which is
// what lets --list and doctor's inventory ask what this build ships.
func TestTheChecksAreConstructedWithoutTouchingTheHost(t *testing.T) {
	for _, c := range Checks() {
		if c.Spec().NeedsBins != nil {
			t.Errorf("%s needs a binary, but neither check shells out", c.Spec().ID)
		}
		if c.Spec().Tier2 {
			t.Errorf("%s is marked tier 2; both read with the permission the operator already has", c.Spec().ID)
		}
	}
}

func TestHumanBytesRoundsPastTheFirstUnit(t *testing.T) {
	cases := map[int64]string{
		0:                "0 B",
		512:              "512 B",
		1024:             "1.0 KB",
		1536:             "1.5 KB",
		10 * 1024 * 1024: "10.0 MB",
	}
	for n, want := range cases {
		if got := humanBytes(n); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestPluralKeepsTheSingularSingular(t *testing.T) {
	if got := plural(1, "file"); got != "1 file" {
		t.Errorf("plural(1) = %q", got)
	}
	if got := plural(2, "file"); got != "2 files" {
		t.Errorf("plural(2) = %q", got)
	}
	if got := plural(0, "file"); got != "0 files" {
		t.Errorf("plural(0) = %q", got)
	}
}
