package storage

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	corestorage "github.com/KingPin/FleetFix/v2/internal/core/storage"
)

const day = 24 * time.Hour

func candidates(t *testing.T, res check.Result) []Candidate {
	t.Helper()
	found, ok := res.Data.([]Candidate)
	if !ok {
		t.Fatalf("data is %T, want []Candidate", res.Data)
	}
	return found
}

func names(t *testing.T, root string, res check.Result) []string {
	t.Helper()
	out := []string{}
	for _, c := range candidates(t, res) {
		rel, err := filepath.Rel(root, c.Path)
		if err != nil {
			t.Fatalf("%s is not under %s: %v", c.Path, root, err)
		}
		out = append(out, filepath.ToSlash(rel))
	}
	return out
}

func TestOnlyTheGlobbedNamesAreCandidates(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{
		"backup.sql":     {size: 300, age: 90 * day},
		"app.log.1":      {size: 200, age: 90 * day},
		"notes.txt":      {size: 400, age: 90 * day},
		"Makefile":       {size: 500, age: 90 * day},
		"archive.tar.gz": {size: 100, age: 90 * day},
	})

	res, _ := runStale(t, root, "30")

	got := names(t, root, res)
	slices.Sort(got)
	want := []string{"app.log.1", "archive.tar.gz", "backup.sql"}
	if !slices.Equal(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

// fnmatchcase, not fnmatch: on a case-sensitive filesystem BACKUP.SQL and
// backup.sql are different files, and v1 matches case.
func TestTheGlobsAreCaseSensitive(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"BACKUP.SQL": {size: 300, age: 90 * day}})

	res, _ := runStale(t, root, "30")

	if got := names(t, root, res); len(got) != 0 {
		t.Errorf("candidates = %v, want none", got)
	}
}

func TestTheCategoryComesFromWhichListMatched(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{
		"dump.sql":  {size: 300, age: 90 * day},
		"app.log.7": {size: 200, age: 90 * day},
	})

	res, _ := runStale(t, root, "30")

	got := map[string]string{}
	for _, c := range candidates(t, res) {
		got[filepath.Base(c.Path)] = c.Category
	}
	if got["dump.sql"] != corestorage.CategoryArtifact {
		t.Errorf("dump.sql is %q, want %q", got["dump.sql"], corestorage.CategoryArtifact)
	}
	if got["app.log.7"] != corestorage.CategoryLog {
		t.Errorf("app.log.7 is %q, want %q", got["app.log.7"], corestorage.CategoryLog)
	}
}

func TestAFileTouchedInsideTheWindowIsNotStale(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{
		"fresh.sql": {size: 300, age: 5 * day},
		"old.sql":   {size: 100, age: 40 * day},
	})

	res, _ := runStale(t, root, "30")

	if got := names(t, root, res); !slices.Equal(got, []string{"old.sql"}) {
		t.Errorf("candidates = %v, want [old.sql]", got)
	}
}

// v1 skips a file whose mtime is strictly greater than the cutoff, so a file
// exactly at the boundary is stale. Asserted because the alternative is a
// one-character difference nobody would notice.
func TestAFileExactlyAtTheCutoffIsStale(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"edge.sql": {size: 300, age: 30 * day}})

	res, _ := runStale(t, root, "30")

	if got := names(t, root, res); !slices.Equal(got, []string{"edge.sql"}) {
		t.Errorf("candidates = %v, want [edge.sql]", got)
	}
}

func TestTheWindowIsTheParameter(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"week.sql": {size: 300, age: 8 * day}})

	if res, _ := runStale(t, root, "30"); len(candidates(t, res)) != 0 {
		t.Error("a file eight days old is stale at thirty days")
	}
	res, _ := runStale(t, root, "7")
	if got := names(t, root, res); !slices.Equal(got, []string{"week.sql"}) {
		t.Errorf("candidates = %v, want [week.sql] at seven days", got)
	}
}

func TestBiggestFirstWithThePathBreakingTies(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{
		"small.sql":  {size: 100, age: 90 * day},
		"b/tie.sql":  {size: 300, age: 90 * day},
		"a/tie.sql":  {size: 300, age: 90 * day},
		"middle.sql": {size: 200, age: 90 * day},
	})

	res, _ := runStale(t, root, "30")

	want := []string{"a/tie.sql", "b/tie.sql", "middle.sql", "small.sql"}
	if got := names(t, root, res); !slices.Equal(got, want) {
		t.Errorf("order = %v, want %v", got, want)
	}
}

func TestThePruneListIsNotDescendedInto(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{
		"node_modules/pkg/huge.tar.gz": {size: 9000, age: 90 * day},
		".cache/dump.sql":              {size: 8000, age: 90 * day},
		".git/objects/pack.zip":        {size: 7000, age: 90 * day},
		"keep/report.sql":              {size: 10, age: 90 * day},
	})

	res, _ := runStale(t, root, "30")

	if got := names(t, root, res); !slices.Equal(got, []string{"keep/report.sql"}) {
		t.Errorf("candidates = %v, want only keep/report.sql", got)
	}
}

// The root is the operator's own instruction. Pointing this at ~/.cache on
// purpose has to work, or the prune list silently overrides the argument.
func TestTheRootIsNeverPrunedByItsOwnName(t *testing.T) {
	parent := tree(t, fixedClock, map[string]staged{".cache/dump.sql": {size: 300, age: 90 * day}})
	root := filepath.Join(parent, ".cache")

	res, _ := runStale(t, root, "30")

	if got := names(t, root, res); !slices.Equal(got, []string{"dump.sql"}) {
		t.Errorf("candidates = %v, want [dump.sql]", got)
	}
}

// What an operator would be agreeing to delete is the link; the bytes the size
// promises to reclaim are the target's.
func TestASymlinkIsNotACandidate(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"real.sql": {size: 300, age: 90 * day}})
	link := filepath.Join(root, "link.sql")
	if err := os.Symlink(filepath.Join(root, "real.sql"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	old := fixedClock.Add(-90 * day)
	if err := os.Chtimes(link, old, old); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	res, _ := runStale(t, root, "30")

	if got := names(t, root, res); !slices.Equal(got, []string{"real.sql"}) {
		t.Errorf("candidates = %v, want only real.sql", got)
	}
}

func TestTheTotalsAreTheAlertingContract(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{
		"big.sql":   {size: 300, age: 90 * day},
		"small.sql": {size: 100, age: 90 * day},
	})

	res, _ := runStale(t, root, "30")

	if got := metricNamed(t, res, StaleBytesMetric).Value; got != 400 {
		t.Errorf("%s = %v, want 400", StaleBytesMetric, got)
	}
	if got := metricNamed(t, res, StaleFilesMetric).Value; got != 2 {
		t.Errorf("%s = %v, want 2", StaleFilesMetric, got)
	}
	if got := metricNamed(t, res, StaleLargestMetric).Value; got != 300 {
		t.Errorf("%s = %v, want 300", StaleLargestMetric, got)
	}
}

func TestTheMetricsAreLabelledByRoot(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"dump.sql": {size: 300, age: 90 * day}})

	res, _ := runStale(t, root, "30")

	for _, m := range res.Metrics {
		if m.Labels["root"] != root {
			t.Errorf("%s is labelled %v, want root=%s", m.Name, m.Labels, root)
		}
	}
}

// A clean directory still reports its zeroes: an absent series is what makes a
// dashboard draw nothing where it should draw a flat line.
func TestACleanRootStillCarriesTheMetrics(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"notes.txt": {size: 300, age: 90 * day}})

	res, _ := runStale(t, root, "30")

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
	if len(res.Metrics) != 3 {
		t.Fatalf("%d metrics, want 3", len(res.Metrics))
	}
	if got := metricNamed(t, res, StaleLargestMetric).Value; got != 0 {
		t.Errorf("%s = %v, want 0", StaleLargestMetric, got)
	}
	if !strings.Contains(res.Summary, "nothing under "+root) {
		t.Errorf("summary = %q", res.Summary)
	}
	if !strings.Contains(res.Summary, "30 days") {
		t.Errorf("summary does not say how old is old: %q", res.Summary)
	}
}

// Nothing this check finds is a fault. See the package doc: disk.usage grades the
// fullness, and four gigabytes of dumps is only a problem relative to it.
func TestAFullRootIsStillOK(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"huge.sql": {size: 5000, age: 900 * day}})

	res, _ := runStale(t, root, "30")

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
	if len(res.Trips) != 0 {
		t.Errorf("trips = %v, want none: the domain grades nothing", res.Trips)
	}
}

func TestTheSummaryNamesTheWorstOffender(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{
		"big.sql":   {size: 2048, age: 90 * day},
		"small.sql": {size: 1024, age: 90 * day},
	})

	res, _ := runStale(t, root, "30")

	want := "2 stale files holding 3.0 KB under " + root +
		", largest is " + filepath.Join(root, "big.sql") + " at 2.0 KB"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

func TestEachCandidateIsNarratedWithItsAge(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"dump.sql": {size: 1024, age: 45 * day}})

	_, steps := runStale(t, root, "30")

	want := filepath.Join(root, "dump.sql") + " is 1.0 KB, 45 days old, artifact"
	if got := texts(steps); !slices.Equal(got, []string{want}) {
		t.Errorf("steps = %v, want [%q]", got, want)
	}
}

func TestTheNarrationIsCappedWithARemainderLine(t *testing.T) {
	files := map[string]staged{}
	for i := range 14 {
		files["dump"+strconv.Itoa(i)+".sql"] = staged{size: 1000 - i, age: 90 * day}
	}
	root := tree(t, fixedClock, files)

	res, steps := runStale(t, root, "30")

	if len(steps) != stepCap+1 {
		t.Fatalf("%d steps, want %d", len(steps), stepCap+1)
	}
	if got := steps[stepCap].Text; got != "and 4 more files" {
		t.Errorf("remainder = %q", got)
	}
	// Capped in the narration only: data[] still carries every one of them.
	if got := len(candidates(t, res)); got != 14 {
		t.Errorf("data holds %d candidates, want 14", got)
	}
}

func TestAMissingRootIsAnError(t *testing.T) {
	root := filepath.Join(t.TempDir(), "nope")

	res, _ := runStale(t, root, "30")

	// Error, not unavailable: the path is the operator's own argument, and a
	// mistyped one that exits zero would stay green across a whole fleet.
	if res.Status != check.StatusError {
		t.Errorf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "no such directory") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestARootThatIsAFileIsAnError(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"file.sql": {size: 10, age: 90 * day}})
	path := filepath.Join(root, "file.sql")

	res, _ := runStale(t, path, "30")

	if res.Status != check.StatusError {
		t.Errorf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "is not a directory") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// ENOTDIR is neither "missing" nor "denied", so it lands in the branch that just
// says what happened rather than pretending to a diagnosis.
func TestARootBeneathAFileIsAnError(t *testing.T) {
	parent := tree(t, fixedClock, map[string]staged{"file.sql": {size: 10, age: 90 * day}})
	root := filepath.Join(parent, "file.sql", "under")

	res, _ := runStale(t, root, "30")

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "could not read "+root) {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.Error == "" {
		t.Error("no error text, so nobody can tell why")
	}
}

func TestAnUnreadableRootIsUnavailable(t *testing.T) {
	skipAsRoot(t)
	root := tree(t, fixedClock, map[string]staged{"dump.sql": {size: 300, age: 90 * day}})
	if err := os.Chmod(root, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o755) })

	res, _ := runStale(t, root, "30")

	// Unavailable rather than ok, and emphatically so: reporting a clean directory
	// because we could not open it is the permanently-green failure this layer
	// exists to prevent.
	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %s, want unavailable", res.Status)
	}
	if !strings.Contains(res.Summary, "not readable by this user") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestAnUnreadableSubtreeIsCountedRatherThanSwallowed(t *testing.T) {
	skipAsRoot(t)
	root := tree(t, fixedClock, map[string]staged{
		"visible.sql":       {size: 100, age: 90 * day},
		"locked/inside.sql": {size: 9000, age: 90 * day},
	})
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, steps := runStale(t, root, "30")

	// The status does not move -- one root-owned directory under a home is
	// ordinary, and a permanent warn for it teaches an operator to ignore the check
	// -- but the reading says it is a floor, in steps[] and in the summary both.
	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok", res.Status)
	}
	if !strings.HasSuffix(res.Summary, "; 1 path could not be read") {
		t.Errorf("summary = %q", res.Summary)
	}
	if len(steps) == 0 || steps[0].Status != check.StatusWarn {
		t.Fatalf("steps = %v, want a warn line first", texts(steps))
	}
	if !strings.Contains(steps[0].Text, "this total is a floor") {
		t.Errorf("first step = %q", steps[0].Text)
	}
}

func TestACleanLookingRootSaysWhenItCouldNotLookEverywhere(t *testing.T) {
	skipAsRoot(t)
	root := tree(t, fixedClock, map[string]staged{"locked/inside.sql": {size: 9000, age: 90 * day}})
	locked := filepath.Join(root, "locked")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	res, _ := runStale(t, root, "30")

	if !strings.HasSuffix(res.Summary, "; 1 path could not be read") {
		t.Errorf("summary = %q, and a bare \"nothing matched\" would be a lie", res.Summary)
	}
}

func TestANonNumericWindowIsAnError(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"dump.sql": {size: 300, age: 90 * day}})

	res, _ := runStale(t, root, "30d")

	// Not a silent fall back to thirty: the operator asked a question this check
	// did not answer.
	if res.Status != check.StatusError {
		t.Errorf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "whole number of days") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestAWalkThatDoesNotFinishIsAnError(t *testing.T) {
	root := tree(t, fixedClock, map[string]staged{"dump.sql": {size: 300, age: 90 * day}})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res := stale{now: func() time.Time { return fixedClock }}.Run(ctx, check.Input{
		Params:   map[string]string{RootParam: root, OlderThanDaysParam: "30"},
		Progress: check.Discard,
	}).Normalize()

	// A partial total is worse than none: a reader cannot tell "1.2 GB" from the
	// whole answer.
	if res.Status != check.StatusError {
		t.Errorf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Summary, "did not finish") {
		t.Errorf("summary = %q", res.Summary)
	}
}

// The clock is a test seam, so the shipped check has to be shown using the real
// one -- a nil now that silently reported nothing would pass every test above.
func TestTheShippedCheckUsesTheLiveClock(t *testing.T) {
	root := tree(t, time.Now(), map[string]staged{"dump.sql": {size: 300, age: 400 * day}})

	var c check.Check
	for _, candidate := range Checks() {
		if candidate.Spec().ID == StaleID {
			c = candidate
		}
	}
	res, _ := runCheck(t, c, map[string]string{RootParam: root, OlderThanDaysParam: "30"})

	if got := names(t, root, res); !slices.Equal(got, []string{"dump.sql"}) {
		t.Errorf("candidates = %v, want [dump.sql]", got)
	}
}

func TestTheWindowDefaultsToAMonth(t *testing.T) {
	for _, p := range (stale{}).Spec().Params {
		if p.Name == OlderThanDaysParam && p.Default != strconv.Itoa(DefaultStaleAgeDays) {
			t.Errorf("%s defaults to %q, want %d", p.Name, p.Default, DefaultStaleAgeDays)
		}
		if p.Name == RootParam && !p.Required {
			t.Error("root is optional, so a bare run would scan somewhere nobody named")
		}
	}
}
