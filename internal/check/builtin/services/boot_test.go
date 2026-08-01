package services

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coreservices "github.com/KingPin/FleetFix/v2/internal/core/services"
	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

func blaming(t *testing.T, name string) *cmdrun.Fake {
	t.Helper()
	fake := cmdrun.NewFake()
	fake.Stdout(fixture.Text(t, name), analyzeBin, blameArgs...)
	return fake
}

// blameText stages arbitrary output, for the shapes the corpus does not hold --
// a host with more slow units than steps[] should carry, mainly.
func blameText(text string) *cmdrun.Fake {
	fake := cmdrun.NewFake()
	fake.Stdout(text, analyzeBin, blameArgs...)
	return fake
}

func TestBootReadsTheCorpus(t *testing.T) {
	res, _ := run(t, BootID, blaming(t, "systemd_analyze/blame_classic.txt"))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	// Slowest-first is systemd's own order, and the parser keeps it, so the first
	// entry is the slowest without a sort of our own.
	want := "4 units timed, slowest is archlinux-keyring-wkd-sync.service at 59.647s"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if got := metricNamed(t, res, SlowestMetric).Value; got != 59647 {
		t.Errorf("%s = %v, want 59647", SlowestMetric, got)
	}
	if got := metricNamed(t, res, SlowestMetric).Unit; got != "ms" {
		t.Errorf("%s is in %q, want ms", SlowestMetric, got)
	}
}

// The slowest metric carries no unit label. Labelling it would mint a new
// Prometheus series every time the slowest unit changed and leave the old one
// stale -- which unit it was belongs in the summary and in data[].
func TestTheSlowestMetricIsNotLabelledByUnit(t *testing.T) {
	res, _ := run(t, BootID, blaming(t, "systemd_analyze/blame_classic.txt"))

	if got := metricNamed(t, res, SlowestMetric).Labels; len(got) != 0 {
		t.Errorf("labels = %v, want none", got)
	}
}

// The corpus host has three units at or over five seconds -- 59.647s, 5.569s and
// 1min 2.234s -- and one under, at 559ms.
func TestOnlyTheSlowUnitsAreCountedAndNarrated(t *testing.T) {
	res, steps := run(t, BootID, blaming(t, "systemd_analyze/blame_classic.txt"))

	if got := metricNamed(t, res, SlowUnitsMetric).Value; got != 3 {
		t.Errorf("%s = %v, want 3", SlowUnitsMetric, got)
	}
	if len(steps) != 3 {
		t.Fatalf("steps = %v, want one per slow unit", texts(steps))
	}
	if want := "archlinux-keyring-wkd-sync.service took 59.647s"; steps[0].Text != want {
		t.Errorf("step = %q, want %q", steps[0].Text, want)
	}
	// The 1min unit is rendered the way systemd-analyze printed it, so this line
	// and the line an operator gets from running the command say the same thing.
	if want := "long-thing.service took 1min 2.234s"; steps[2].Text != want {
		t.Errorf("step = %q, want %q", steps[2].Text, want)
	}
	if strings.Contains(strings.Join(texts(steps), "\n"), "NetworkManager.service took") {
		t.Errorf("the 559ms unit was narrated: %v", texts(steps))
	}
}

// A reading this check does not grade is narrated at ok. A warn line under an ok
// result would read as a fault the status forgot to mention.
func TestSlowUnitsDoNotDowngradeTheHost(t *testing.T) {
	res, steps := run(t, BootID, blaming(t, "systemd_analyze/blame_classic.txt"))

	if res.Status != check.StatusOK {
		t.Errorf("status = %s, want ok; there is no boot rule to trip", res.Status)
	}
	if len(res.Trips) != 0 {
		t.Errorf("trips = %+v; this check grades nothing", res.Trips)
	}
	for _, s := range steps {
		if s.Status != check.StatusOK {
			t.Errorf("step %q has status %s, want ok", s.Text, s.Status)
		}
	}
}

// No total. Summing these is not the boot time -- systemd starts units in
// parallel, so the sum exceeds the wall clock by however much overlapped -- and a
// number labelled "boot" that is not how long the boot took is worse than none.
func TestBootReportsNoTotal(t *testing.T) {
	res, _ := run(t, BootID, blaming(t, "systemd_analyze/blame_classic.txt"))

	if len(res.Metrics) != 2 {
		t.Fatalf("metrics = %v, want exactly the slowest and the count", res.Metrics)
	}
	for _, m := range res.Metrics {
		if strings.Contains(m.Name, "total") {
			t.Errorf("%s looks like a boot total", m.Name)
		}
	}
}

// data[] carries every entry, not only the slow ones: an operator reading a slow
// boot wants the whole ladder, and the corpus's blank lines are the parser's
// business rather than this layer's.
func TestBootDataCarriesEveryEntry(t *testing.T) {
	res, _ := run(t, BootID, blaming(t, "systemd_analyze/blame_blank_lines.txt"))

	entries, ok := res.Data.([]coreservices.BlameEntry)
	if !ok {
		t.Fatalf("data is %T, want []coreservices.BlameEntry", res.Data)
	}
	if len(entries) == 0 {
		t.Fatal("data carries no entries")
	}
}

// A host with more slow units than steps[] should carry says how many it left
// out. Truncating quietly would read as "these are all of them".
func TestTheStepCapNamesWhatItLeftOut(t *testing.T) {
	var b strings.Builder
	for i := range stepCap + 3 {
		fmt.Fprintf(&b, "%d.500s unit%02d.service\n", 30-i, i)
	}

	res, steps := run(t, BootID, blameText(b.String()))

	if got := metricNamed(t, res, SlowUnitsMetric).Value; got != float64(stepCap+3) {
		t.Errorf("%s = %v, want every slow unit counted", SlowUnitsMetric, got)
	}
	if len(steps) != stepCap+1 {
		t.Fatalf("steps = %d, want the cap plus the remainder line", len(steps))
	}
	if want := "and 3 more units over 5.000s"; steps[stepCap].Text != want {
		t.Errorf("last step = %q, want %q", steps[stepCap].Text, want)
	}
}

// Exactly at the cap there is nothing left over, so no remainder line.
func TestNoRemainderLineWhenEverySlowUnitFits(t *testing.T) {
	var b strings.Builder
	for i := range stepCap {
		fmt.Fprintf(&b, "%d.500s unit%02d.service\n", 30-i, i)
	}

	_, steps := run(t, BootID, blameText(b.String()))

	if len(steps) != stepCap {
		t.Fatalf("steps = %d, want exactly the cap", len(steps))
	}
	if strings.Contains(steps[stepCap-1].Text, "more units") {
		t.Errorf("last step = %q, want a unit rather than a remainder", steps[stepCap-1].Text)
	}
}

// A host that booted has timings, so an empty answer means nothing measured this
// boot -- an nspawn container, a systemd too old to have kept them. ok would
// report a fast boot on a host nobody timed.
func TestAHostWithNoTimingsIsUnavailableRatherThanFast(t *testing.T) {
	res, steps := run(t, BootID, blameText("\n\n"))

	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %s, want unavailable", res.Status)
	}
	if want := "this host reports no boot timings"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(res.Metrics) != 0 || len(steps) != 0 {
		t.Errorf("metrics = %v, steps = %v for a host with nothing to report", res.Metrics, texts(steps))
	}
}

// systemd-analyze refuses while the boot is still finishing, and inside a
// container with no boot to analyse. Both say so on stderr, and both are worth
// reporting in systemd's own words rather than as "no timings".
func TestARefusedBlameIsAnErrorInSystemdsOwnWords(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Exit(1, "", "Bootup is not yet finished", analyzeBin, blameArgs...)

	res, _ := run(t, BootID, fake)

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Error, "Bootup is not yet finished") {
		t.Errorf("error = %q", res.Error)
	}
}

func TestABlameThatDidNotRunIsAnError(t *testing.T) {
	fake := cmdrun.NewFake()
	fake.Fail(errors.New("context deadline exceeded"), analyzeBin, blameArgs...)

	res, _ := run(t, BootID, fake)

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error", res.Status)
	}
	if !strings.Contains(res.Error, "deadline") {
		t.Errorf("error = %q", res.Error)
	}
}

func TestDuration(t *testing.T) {
	for _, tc := range []struct {
		ms   int64
		want string
	}{
		{0, "0ms"},
		{559, "559ms"},
		{999, "999ms"},
		{1000, "1.000s"},
		{5569, "5.569s"},
		{59647, "59.647s"},
		{60000, "1min 0.000s"},
		{62234, "1min 2.234s"},
		{3_723_000, "62min 3.000s"},
	} {
		if got := duration(tc.ms); got != tc.want {
			t.Errorf("duration(%d) = %q, want %q", tc.ms, got, tc.want)
		}
	}
}
