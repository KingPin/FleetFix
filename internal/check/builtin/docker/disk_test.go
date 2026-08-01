package docker

import (
	"errors"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coredocker "github.com/KingPin/FleetFix/v2/internal/core/docker"
)

func diskOn(fake *cmdrun.Fake) diskUsage {
	return diskUsage{run: fake, runtime: liveDocker()}
}

func dfHost(out string) *cmdrun.Fake {
	fake := cmdrun.NewFake()
	fake.Stdout(out, "docker", dfArgs...)
	return fake
}

// ok whatever the numbers say. internal/threshold has no docker rule, so there is
// nothing to grade against -- 45GB reclaimable is not high or low without knowing
// the disk under it, and a bound invented here would be warn on every build host.
func TestDiskReportsTheTotalsWithoutGradingThem(t *testing.T) {
	res, _ := run(t, diskOn(dfHost(dfFourCategories)))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok", res.Status)
	}
	if len(res.Trips) != 0 {
		t.Errorf("trips = %v; this domain grades nothing", res.Trips)
	}
	// Docker's sizes are decimal and v1's renderer divides by 1024, so 2.67GB +
	// 136B + 512MB + 48.3MB reads as 3.0 GB rather than 3.2.
	want := "3.0 GB across 4 categories, 1.6 GB reclaimable"
	if res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

// One line per category, in docker's own order, with the three facts an operator
// prunes on.
func TestEveryCategoryGetsALine(t *testing.T) {
	_, rec := run(t, diskOn(dfHost(dfFourCategories)))

	want := []string{
		"Images: 2 of 7 active, 2.5 GB, 1.1 GB reclaimable (45%)",
		"Containers: 1 of 2 active, 136 B, nothing reclaimable",
		"Local Volumes: 1 of 3 active, 488.3 MB, 488.3 MB reclaimable",
		"Build Cache: 0 of 11 active, 46.1 MB, 46.1 MB reclaimable",
	}
	got := rec.texts()
	if len(got) != len(want) {
		t.Fatalf("steps = %v, want one per category", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("step %d\n got %q\nwant %q", i, got[i], want[i])
		}
	}
	for _, e := range rec.events {
		if e.Status != check.StatusOK {
			t.Errorf("step %q is %s; nothing here is graded", e.Text, e.Status)
		}
	}
}

// The percentage is omitted rather than printed as a zero. ParseReclaimablePct
// cannot tell a real 0% from a column docker printed without one, and "(0%)" beside
// a non-zero size reads as "none" -- the opposite of what it means.
func TestAnAbsentPercentageIsLeftOutRatherThanPrintedAsZero(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  coredocker.DfRow
		want string
	}{
		{
			"no percentage in the column",
			coredocker.DfRow{Type: "Local Volumes", Active: 1, TotalCount: 3, SizeBytes: 512_000_000, ReclaimableBytes: 512_000_000},
			"Local Volumes: 1 of 3 active, 488.3 MB, 488.3 MB reclaimable",
		},
		{
			"nothing to reclaim",
			coredocker.DfRow{Type: "Containers", Active: 1, TotalCount: 2, SizeBytes: 136},
			"Containers: 1 of 2 active, 136 B, nothing reclaimable",
		},
		{
			// docker names every category it prints; an unnamed one is a line we
			// read but cannot label, and dropping it would lose its size from a
			// total that has to add up.
			"a category docker did not name",
			coredocker.DfRow{Active: 0, TotalCount: 1, SizeBytes: 1024, ReclaimableBytes: 1024, ReclaimablePct: 100},
			"unnamed category: 0 of 1 active, 1.0 KB, 1.0 KB reclaimable (100%)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := describe(tc.row); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// Three series per category, labelled by type: what it holds, what a prune returns,
// and the share. This is the alerting contract -- data[] is best-effort, metrics[]
// is not.
func TestEachCategoryBecomesThreeLabelledSeries(t *testing.T) {
	res, _ := run(t, diskOn(dfHost(dfFourCategories)))

	if len(res.Metrics) != 12 {
		t.Fatalf("metrics = %d, want three for each of four categories", len(res.Metrics))
	}
	for name, want := range map[string]float64{
		DiskBytesMetric:   2_670_000_000,
		ReclaimableMetric: 1_200_000_000,
		ReclaimablePct:    45,
	} {
		m := metricNamed(t, res, name, "Images")
		if m.Value != want {
			t.Errorf("%s{Images} = %v, want %v", name, m.Value, want)
		}
		if m.Kind != check.Gauge {
			t.Errorf("%s is a %s, want a gauge", name, m.Kind)
		}
	}
	// data[] carries the parsed rows for the TUI's table.
	if rows, ok := res.Data.([]coredocker.DfRow); !ok || len(rows) != 4 {
		t.Errorf("data = %#v, want the four parsed rows", res.Data)
	}
}

// A working daemon always prints its categories, even empty ones. Nothing parsed
// means the output was not what this build reads -- a dialect change, or a podman
// behind a docker-named symlink -- and calling that ok would report a host with a
// full disk as clean.
func TestNoParsableCategoriesIsAnError(t *testing.T) {
	res, _ := run(t, diskOn(dfHost("TYPE   TOTAL   ACTIVE   SIZE   RECLAIMABLE\n")))

	if res.Status != check.StatusError {
		t.Errorf("status = %s, want error", res.Status)
	}
	if res.Summary != "docker system df reported no categories" {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.Error == "" {
		t.Error("no error text, so nothing says why")
	}
}

func TestADfThatWouldNotRunIsAnError(t *testing.T) {
	t.Run("did not run", func(t *testing.T) {
		fake := cmdrun.NewFake()
		fake.Fail(errors.New("context deadline exceeded"), "docker", dfArgs...)

		res, _ := run(t, diskOn(fake))

		if res.Status != check.StatusError || res.Summary != "docker system df did not run" {
			t.Errorf("status = %s, summary = %q", res.Status, res.Summary)
		}
		if res.Error != "context deadline exceeded" {
			t.Errorf("error = %q", res.Error)
		}
	})

	t.Run("exited non-zero", func(t *testing.T) {
		fake := cmdrun.NewFake()
		fake.Exit(1, "", "Error response from daemon: server error", "docker", dfArgs...)

		res, _ := run(t, diskOn(fake))

		if res.Status != check.StatusError || res.Summary != "docker system df failed" {
			t.Errorf("status = %s, summary = %q", res.Status, res.Summary)
		}
		if res.Error != "Error response from daemon: server error" {
			t.Errorf("error = %q", res.Error)
		}
	})

	t.Run("exited non-zero saying nothing", func(t *testing.T) {
		fake := cmdrun.NewFake()
		fake.Exit(2, "", "", "docker", dfArgs...)

		if res, _ := run(t, diskOn(fake)); res.Error != "exited 2" {
			t.Errorf("error = %q", res.Error)
		}
	})
}

func TestDiskAsksNothingOfADeadDaemon(t *testing.T) {
	fake := cmdrun.NewFake()

	res, _ := run(t, diskUsage{run: fake, runtime: deadDocker()})

	if res.Status != check.StatusUnavailable {
		t.Errorf("status = %s, want unavailable", res.Status)
	}
	if !strings.Contains(res.Summary, "unix:///var/run/docker.sock") {
		t.Errorf("summary = %q, want the daemon's own reason", res.Summary)
	}
	if len(fake.Calls()) != 0 {
		t.Errorf("calls = %v", fake.Calls())
	}
}

// The slowest question in the domain: `system df` walks every layer and volume.
func TestTheDiskCheckCarriesV1sTimeout(t *testing.T) {
	if spec := (diskUsage{}).Spec(); spec.Budget != dfBudget {
		t.Errorf("budget = %s, want %s", spec.Budget, dfBudget)
	}
}

// A single category still reads as a sentence rather than "1 categorys".
func TestOneCategoryIsSingular(t *testing.T) {
	res, _ := run(t, diskOn(dfHost(`{"Active":"0","Reclaimable":"0B","Size":"0B","TotalCount":"0","Type":"Images"}`+"\n")))

	if res.Summary != "0 B across 1 category, 0 B reclaimable" {
		t.Errorf("summary = %q", res.Summary)
	}
}
