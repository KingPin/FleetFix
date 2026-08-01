package disk

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coredisk "github.com/KingPin/FleetFix/v2/internal/core/disk"
	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
	"github.com/google/go-cmp/cmp"
)

// The fixtures are the M0 corpus, the same bytes internal/core/disk's table tests
// and the Python oracle read. Nothing here restates what the parser produces --
// that is already pinned against Python -- so these tests are about the layer the
// port adds: which status, which trips, which metrics, and what the argv was.

func byID(t *testing.T, id check.ID, runner cmdrun.Runner) check.Check {
	t.Helper()
	for _, c := range Checks(runner) {
		if c.Spec().ID == id {
			return c
		}
	}
	t.Fatalf("no check with id %s", id)
	return nil
}

// run drives one check through a staged df and returns the normalized Result.
//
// Normalize because that is what the runner does to everything on its way out,
// and asserting on a Result the report would never carry tests a shape nothing
// ships.
func run(t *testing.T, id check.ID, runner *cmdrun.Fake, set threshold.Set) (check.Result, []check.Event) {
	t.Helper()
	var steps []check.Event
	res := byID(t, id, runner).Run(t.Context(), check.Input{
		Params:     map[string]string{},
		Progress:   check.EmitterFunc(func(e check.Event) { steps = append(steps, e) }),
		Thresholds: set,
	})
	return res.Normalize(), steps
}

// stagedDF answers `df -P <flag>` with a fixture and nothing else, so a check
// reaching for a different argv fails loudly rather than parsing an empty string.
func stagedDF(t *testing.T, flag, name string) *cmdrun.Fake {
	t.Helper()
	return cmdrun.NewFake().Stdout(fixture.Text(t, name), "df", "-P", flag)
}

func metricNamed(res check.Result, name, mount string) (check.Metric, bool) {
	for _, m := range res.Metrics {
		if m.Name == name && m.Labels["mount"] == mount {
			return m, true
		}
	}
	return check.Metric{}, false
}

func tripFor(res check.Result, mount string) (threshold.Trip, bool) {
	for _, tr := range res.Trips {
		if tr.Subject == mount {
			return tr, true
		}
	}
	return threshold.Trip{}, false
}

// The two specs are public API, so the fields a consumer selects on and reads out
// of --list are asserted rather than left to whatever the constructor happened to
// set.
func TestTheSpecsDescribeWhatTheseChecksNeed(t *testing.T) {
	for _, id := range []check.ID{UsageID, InodesID} {
		spec := byID(t, id, cmdrun.NewFake()).Spec()

		if err := spec.Validate(); err != nil {
			t.Errorf("%s: %v", id, err)
		}
		if spec.Domain != "disk" {
			t.Errorf("%s: domain = %q, want disk", id, spec.Domain)
		}
		if got := spec.NeedsBins; len(got) != 1 || got[0] != "df" {
			t.Errorf("%s: needs_bins = %v, want [df]", id, got)
		}
		if spec.Tier2 {
			t.Errorf("%s: marked tier 2; df needs no privilege and marking it so would "+
				"skip the check on every non-root host", id)
		}
		if !spec.InDefault {
			t.Errorf("%s: not in the default set, so a bare `fleetfix check` would not "+
				"look at the disks", id)
		}
		// v1's run_df and run_df_inodes both default to timeout_s=5.
		if spec.Budget != 5*time.Second {
			t.Errorf("%s: budget = %s, want v1's 5s df timeout", id, spec.Budget)
		}
		if len(spec.Params) != 0 {
			t.Errorf("%s: takes parameters, which would make it operator-driven and "+
				"exclude it from a default run", id)
		}
	}
}

func TestAHealthyHostGradesOK(t *testing.T) {
	// A single mount at 50%, well under the 85 warn bound.
	res, steps := run(t, UsageID,
		stagedDF(t, "-k", "df/usage_mount_with_spaces.txt"), threshold.Defaults())

	if res.Status != check.StatusOK {
		t.Errorf("status = %q, want ok", res.Status)
	}
	if len(res.Trips) != 0 {
		t.Errorf("trips = %v, want none", res.Trips)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v, want none: df said nothing to report", steps)
	}
}

// The mixed fixture has /var at 90% and /var/lib/docker at 96%, so it crosses
// both bounds at once and the worst of the two has to win.
func TestTheWorstMountSetsTheStatus(t *testing.T) {
	res, _ := run(t, UsageID, stagedDF(t, "-k", "df/usage_mixed.txt"), threshold.Defaults())

	if res.Status != check.StatusCrit {
		t.Errorf("status = %q, want crit: /var/lib/docker is at 96%%", res.Status)
	}
	if len(res.Trips) != 2 {
		t.Fatalf("trips = %+v, want two", res.Trips)
	}

	warn, ok := tripFor(res, "/var")
	if !ok {
		t.Fatalf("no trip for /var in %+v", res.Trips)
	}
	want := threshold.Trip{
		Rule: threshold.DiskUsedPct, Severity: threshold.Warn, Status: "warn",
		Value: 90, Bound: 85, Unit: "%", Subject: "/var",
	}
	if diff := cmp.Diff(want, warn); diff != "" {
		t.Errorf("/var trip (-want +got):\n%s", diff)
	}

	crit, ok := tripFor(res, "/var/lib/docker")
	if !ok || crit.Severity != threshold.Crit || crit.Bound != 95 {
		t.Errorf("/var/lib/docker trip = %+v, want crit against the 95 bound", crit)
	}
	// The mount at 50% is graded and does not trip, which is the distinction
	// between "nothing to say" and "not looked at".
	if _, tripped := tripFor(res, "/"); tripped {
		t.Error("/ tripped at 50%")
	}
}

func TestEveryMountGetsBothMetrics(t *testing.T) {
	res, _ := run(t, UsageID, stagedDF(t, "-k", "df/usage_mixed.txt"), threshold.Defaults())

	// Three real mounts survive the parser's pseudo-filesystem and zero-capacity
	// skips, two metrics each.
	if len(res.Metrics) != 6 {
		t.Errorf("%d metrics, want 6:\n%+v", len(res.Metrics), res.Metrics)
	}

	pct, ok := metricNamed(res, UsedPctMetric, "/var")
	if !ok {
		t.Fatalf("no %s for /var", UsedPctMetric)
	}
	want := check.Metric{
		Name: UsedPctMetric, Value: 90, Unit: "%",
		Labels: map[string]string{"mount": "/var"},
		Kind:   check.Gauge, Help: "filesystem capacity used",
	}
	if diff := cmp.Diff(want, pct); diff != "" {
		t.Errorf("%s (-want +got):\n%s", UsedPctMetric, diff)
	}

	// The parsers keep df's 1024-byte blocks; the metric is bytes.
	avail, ok := metricNamed(res, AvailBytesMetric, "/var")
	if !ok {
		t.Fatalf("no %s for /var", AvailBytesMetric)
	}
	if avail.Value != 5242880*1024 {
		t.Errorf("%s = %v, want the fixture's 5242880 blocks as bytes", AvailBytesMetric, avail.Value)
	}
	if avail.Unit != "bytes" {
		t.Errorf("%s unit = %q, want bytes", AvailBytesMetric, avail.Unit)
	}
}

func TestTheSummaryNamesTheFullestMount(t *testing.T) {
	res, _ := run(t, UsageID, stagedDF(t, "-k", "df/usage_mixed.txt"), threshold.Defaults())

	if want := "3 filesystems; fullest is /var/lib/docker at 96%"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

// One mount is "1 filesystem", not "1 filesystems". Cosmetic, but the summary is
// what an operator reads first and what a TUI row shows.
func TestOneMountReadsAsOne(t *testing.T) {
	res, _ := run(t, UsageID, stagedDF(t, "-k", "df/usage_mount_with_spaces.txt"), threshold.Defaults())

	if !strings.HasPrefix(res.Summary, "1 filesystem;") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// The parsed rows go out as data, so a consumer that wants the whole df table
// rather than the graded verdict has it without a second invocation.
func TestTheRowsAreTheData(t *testing.T) {
	res, _ := run(t, UsageID, stagedDF(t, "-k", "df/usage_mixed.txt"), threshold.Defaults())

	rows, ok := res.Data.([]coredisk.Usage)
	if !ok {
		t.Fatalf("data is %T, want the parsed rows", res.Data)
	}
	if len(rows) != 3 || rows[0].Mount != "/" {
		t.Errorf("data = %+v", rows)
	}
}

// -P is not decoration: it is what guarantees six columns and one line per row,
// and it is what the fixture corpus was captured with.
func TestTheArgvIsThePosixOne(t *testing.T) {
	for _, tc := range []struct {
		id   check.ID
		flag string
		name string
	}{
		{UsageID, "-k", "df/usage_mixed.txt"},
		{InodesID, "-i", "df/inodes_mixed.txt"},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			runner := stagedDF(t, tc.flag, tc.name)
			run(t, tc.id, runner, threshold.Defaults())

			if !runner.Called("df", "-P", tc.flag) {
				t.Errorf("ran %v, want df -P %s", runner.Calls(), tc.flag)
			}
		})
	}
}

func TestInodesGradeAgainstTheirOwnRule(t *testing.T) {
	res, _ := run(t, InodesID, stagedDF(t, "-i", "df/inodes_mixed.txt"), threshold.Defaults())

	if res.Status != check.StatusCrit {
		t.Errorf("status = %q, want crit", res.Status)
	}
	for _, tr := range res.Trips {
		// Separate rules even though v1 gave them identical numbers: an operator
		// may want a tighter inode bound, and grading inodes by the byte rule
		// would make that impossible to express.
		if tr.Rule != threshold.DiskInodePct {
			t.Errorf("trip rule = %q, want %q", tr.Rule, threshold.DiskInodePct)
		}
	}
	if want := "3 filesystems; fullest is /var/lib/docker at 96% of inodes"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if _, ok := metricNamed(res, InodeFreeMetric, "/var"); !ok {
		t.Errorf("no %s for /var:\n%+v", InodeFreeMetric, res.Metrics)
	}
}

// A tightened bound has to reach the grading, or thresholds.yml is decoration.
func TestTheOperatorsBoundsAreWhatGrades(t *testing.T) {
	set := threshold.Defaults()
	set[threshold.DiskUsedPct] = threshold.Rule{
		ID: threshold.DiskUsedPct, Warn: 40, Crit: 45, Unit: "%",
	}

	res, _ := run(t, UsageID, stagedDF(t, "-k", "df/usage_mixed.txt"), set)

	// Under the shipped policy the 50% mount is silent; under this one it is
	// critical, and all three mounts trip.
	if len(res.Trips) != 3 {
		t.Errorf("trips = %+v, want all three mounts", res.Trips)
	}
	if trip, ok := tripFor(res, "/"); !ok || trip.Bound != 45 {
		t.Errorf("/ trip = %+v, want the operator's 45 bound", trip)
	}
}

// The permanently-green trap: a policy missing the rule cannot say whether 96% is
// bad, and reporting ok there would be the most misleading answer available.
func TestAPolicyWithNoRuleIsAnError(t *testing.T) {
	for _, tc := range []struct {
		id   check.ID
		rule string
	}{
		{UsageID, threshold.DiskUsedPct},
		{InodesID, threshold.DiskInodePct},
	} {
		t.Run(tc.rule, func(t *testing.T) {
			set := threshold.Defaults()
			delete(set, tc.rule)

			// A bare fake: reaching df at all would mean the check spent a
			// subprocess on a question it could not have graded.
			res, _ := run(t, tc.id, cmdrun.NewFake(), set)

			if res.Status != check.StatusError {
				t.Errorf("status = %q, want error", res.Status)
			}
			if !strings.Contains(res.Error, tc.rule) {
				t.Errorf("error = %q, want it to name the missing rule", res.Error)
			}
		})
	}
}

func TestAMissingDFIsUnavailableNotAnError(t *testing.T) {
	runner := cmdrun.NewFake().Missing("df", "-P", "-k")

	res, _ := run(t, UsageID, runner, threshold.Defaults())

	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %q, want unavailable: a host without df is not a broken host", res.Status)
	}
	if res.Error != "" {
		t.Errorf("error = %q, want empty; nothing went wrong", res.Error)
	}
}

func TestADFThatCouldNotRunIsAnError(t *testing.T) {
	runner := cmdrun.NewFake().Fail(errors.New("context deadline exceeded"), "df", "-P", "-k")

	res, _ := run(t, UsageID, runner, threshold.Defaults())

	if res.Status != check.StatusError {
		t.Errorf("status = %q, want error", res.Status)
	}
	if !strings.Contains(res.Error, "deadline") {
		t.Errorf("error = %q, want the underlying reason", res.Error)
	}
}

// df exits 1 having printed every mount it could read when one is unreadable. v1
// discarded the whole table there; the rows are graded and the complaint is a
// step, because a stale NFS handle must not put a permanent warn on a host whose
// disks are fine.
func TestAPartialDFStillGradesWhatItGot(t *testing.T) {
	runner := cmdrun.NewFake().Exit(1,
		fixture.Text(t, "df/usage_mixed.txt"),
		"df: /mnt/stale: Stale file handle\n",
		"df", "-P", "-k")

	res, steps := run(t, UsageID, runner, threshold.Defaults())

	if res.Status != check.StatusCrit {
		t.Errorf("status = %q, want the verdict on the mounts df did report", res.Status)
	}
	if len(steps) != 1 || !strings.Contains(steps[0].Text, "Stale file handle") {
		t.Fatalf("steps = %+v, want df's complaint", steps)
	}
	if steps[0].Status != check.StatusWarn {
		t.Errorf("step status = %q, want warn", steps[0].Status)
	}
	if res.Error != "" {
		t.Errorf("error = %q; the check did not fail", res.Error)
	}
}

// The same non-zero exit with nothing usable printed is a different answer: df
// was stopped from telling us, which is a fault rather than an absence.
func TestAFailedDFWithNoRowsIsAnError(t *testing.T) {
	runner := cmdrun.NewFake().Exit(1, "", "df: cannot read table of mounted file systems\n", "df", "-P", "-k")

	res, _ := run(t, UsageID, runner, threshold.Defaults())

	if res.Status != check.StatusError {
		t.Errorf("status = %q, want error", res.Status)
	}
	if !strings.Contains(res.Error, "cannot read table") {
		t.Errorf("error = %q, want df's complaint", res.Error)
	}
}

// A non-zero exit that printed nothing on either stream still has to say
// something, or the report carries an error with no reason in it.
func TestASilentFailureStillNamesTheExitCode(t *testing.T) {
	runner := cmdrun.NewFake().Exit(2, "", "", "df", "-P", "-k")

	res, _ := run(t, UsageID, runner, threshold.Defaults())

	if !strings.Contains(res.Error, "exited 2") {
		t.Errorf("error = %q, want the exit code", res.Error)
	}
}

// A container whose only mounts are overlay and tmpfs -- all skipped by the
// parser -- has genuinely nothing to grade. Not a fault, and not ok either: ok
// would claim the disks were checked.
func TestNoRealFilesystemsIsUnavailable(t *testing.T) {
	for _, tc := range []struct {
		id   check.ID
		flag string
	}{
		{UsageID, "-k"},
		{InodesID, "-i"},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			runner := cmdrun.NewFake().Stdout(
				"Filesystem 1024-blocks Used Available Capacity Mounted on\n"+
					"overlay 12345678 1000000 11345678 8% /\n",
				"df", "-P", tc.flag,
			)

			res, _ := run(t, tc.id, runner, threshold.Defaults())

			if res.Status != check.StatusUnavailable {
				t.Errorf("status = %q, want unavailable", res.Status)
			}
			if res.Error != "" {
				t.Errorf("error = %q, want empty", res.Error)
			}
		})
	}
}

// The wire rules, at the layer that produces the values: nil marshals to null and
// [] does not, and a consumer iterating a null gets an error rather than nothing.
func TestEveryResultSurvivesNormalization(t *testing.T) {
	res, _ := run(t, UsageID, cmdrun.NewFake().Missing("df", "-P", "-k"), threshold.Defaults())

	switch {
	case res.Trips == nil:
		t.Error("trips is nil")
	case res.Metrics == nil:
		t.Error("metrics is nil")
	case res.Steps == nil:
		t.Error("steps is nil")
	}
}

// Every metric this package emits carries a mount label, so a scrape can tell two
// series apart. An unlabelled duplicate would silently overwrite in Prometheus.
func TestNoMetricIsUnlabelled(t *testing.T) {
	for _, tc := range []struct {
		id   check.ID
		flag string
		name string
	}{
		{UsageID, "-k", "df/usage_mixed.txt"},
		{InodesID, "-i", "df/inodes_mixed.txt"},
	} {
		t.Run(string(tc.id), func(t *testing.T) {
			res, _ := run(t, tc.id, stagedDF(t, tc.flag, tc.name), threshold.Defaults())

			seen := map[string]bool{}
			for _, m := range res.Metrics {
				if m.Labels["mount"] == "" {
					t.Errorf("%s has no mount label", m.Name)
				}
				if m.Kind != check.Gauge {
					t.Errorf("%s is %q; nothing here counts monotonically", m.Name, m.Kind)
				}
				if m.Help == "" {
					t.Errorf("%s has no help text, which is a blank HELP line in --prom", m.Name)
				}
				key := m.Name + "\x00" + m.Labels["mount"]
				if seen[key] {
					t.Errorf("%s is emitted twice for %s", m.Name, m.Labels["mount"])
				}
				seen[key] = true
			}
		})
	}
}

// Checks hands back independent values, so two front doors building their own
// registries cannot share mutable state through this package.
func TestChecksReturnsTheTwoChecks(t *testing.T) {
	got := Checks(cmdrun.NewFake())

	if len(got) != 2 {
		t.Fatalf("%d checks, want 2", len(got))
	}
	ids := []check.ID{got[0].Spec().ID, got[1].Spec().ID}
	if diff := cmp.Diff([]check.ID{UsageID, InodesID}, ids); diff != "" {
		t.Errorf("ids (-want +got):\n%s", diff)
	}
}

// The budget is the runner's to enforce, but a check that ignores a cancelled
// context would keep a cron job open past it. The seam propagates it; this is the
// assertion that the check does not paper over the failure.
func TestACancelledContextIsAnError(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	runner := cmdrun.NewFake().Fail(context.Canceled, "df", "-P", "-k")
	res := byID(t, UsageID, runner).Run(ctx, check.Input{
		Progress:   check.Discard,
		Thresholds: threshold.Defaults(),
	}).Normalize()

	if res.Status != check.StatusError {
		t.Errorf("status = %q, want error", res.Status)
	}
}
