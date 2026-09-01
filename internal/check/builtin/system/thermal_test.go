package system

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// A laptop-shaped host: a warm CPU package and a cool chassis.
func warmHost() fstest.MapFS {
	return sysWithZones(
		zone{name: "thermal_zone0", kind: "x86_pkg_temp", milli: 55123},
		zone{name: "thermal_zone1", kind: "acpitz", milli: 42000},
	)
}

func TestThermalReportsEveryZoneAndNamesTheHottest(t *testing.T) {
	res, steps := run(t, ThermalID, staged(fstest.MapFS{}, warmHost()))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	if want := "2 thermal zones, hottest is x86_pkg_temp at 55.1C"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(steps) != 0 {
		t.Errorf("steps = %v; a dozen sensor lines per host is noise, not narration", steps)
	}
	if len(res.Metrics) != 2 {
		t.Fatalf("reported %d metrics, want one per zone", len(res.Metrics))
	}
	if TempMetric != threshold.ThermalTempC {
		t.Errorf("the metric is %q and the rule is %q", TempMetric, threshold.ThermalTempC)
	}
}

// Both labels, because neither identifies a sensor alone: the name is stable and
// meaningless, the type is readable and repeats on a multi-socket host.
func TestEachZoneCarriesItsNameAndItsType(t *testing.T) {
	res, _ := run(t, ThermalID, staged(fstest.MapFS{}, warmHost()))

	want := map[string]struct {
		kind string
		temp float64
	}{
		"thermal_zone0": {"x86_pkg_temp", 55.123},
		"thermal_zone1": {"acpitz", 42},
	}
	for _, m := range res.Metrics {
		w, ok := want[m.Labels["zone"]]
		if !ok {
			t.Errorf("unexpected zone %q", m.Labels["zone"])
			continue
		}
		if m.Labels["type"] != w.kind {
			t.Errorf("%s is typed %q, want %q", m.Labels["zone"], m.Labels["type"], w.kind)
		}
		if m.Value != w.temp {
			t.Errorf("%s = %v, want %v", m.Labels["zone"], m.Value, w.temp)
		}
		if m.Unit != "C" {
			t.Errorf("%s is in %q", m.Labels["zone"], m.Unit)
		}
	}
}

// v1's bounds: warn at 70C, crit at 85C -- and the verdict is the hottest zone's
// alone. Averaging a hot package against a cool chassis is how a thermal problem
// stays green, and a host throttles on whichever sensor crosses first.
func TestThermalGradesTheHottestZoneNotTheAverage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		zones   []zone
		status  check.Status
		subject string
	}{
		{
			"one hot package beside three cool zones",
			[]zone{
				{name: "thermal_zone0", kind: "acpitz", milli: 30000},
				{name: "thermal_zone1", kind: "acpitz", milli: 30000},
				{name: "thermal_zone2", kind: "acpitz", milli: 30000},
				{name: "thermal_zone3", kind: "x86_pkg_temp", milli: 88000},
			},
			check.StatusCrit, "x86_pkg_temp",
		},
		{
			"at the warn bound",
			[]zone{{name: "thermal_zone0", kind: "cpu-thermal", milli: 70000}},
			check.StatusWarn, "cpu-thermal",
		},
		{
			"just under it",
			[]zone{{name: "thermal_zone0", kind: "cpu-thermal", milli: 69999}},
			check.StatusOK, "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, _ := run(t, ThermalID, staged(fstest.MapFS{}, sysWithZones(tc.zones...)))
			if res.Status != tc.status {
				t.Errorf("status = %s, want %s (summary %q)", res.Status, tc.status, res.Summary)
			}
			if tc.subject == "" {
				if len(res.Trips) != 0 {
					t.Errorf("trips = %+v", res.Trips)
				}
				return
			}
			if len(res.Trips) != 1 {
				t.Fatalf("trips = %+v, want exactly one -- the hottest zone", res.Trips)
			}
			// The type, not the sysfs name: a trip an operator reads should say
			// which sensor got hot, not which directory it was in.
			if res.Trips[0].Subject != tc.subject {
				t.Errorf("trip subject = %q, want %q", res.Trips[0].Subject, tc.subject)
			}
		})
	}
}

// Every VM and every container reports no thermal zones, and that is most of a
// fleet. unavailable is the only honest answer: a fault would make the common
// case red, and ok would claim a host is running cool when nothing measured it.
func TestAHostWithNoSensorsIsUnavailableRatherThanCoolOrBroken(t *testing.T) {
	res, _ := run(t, ThermalID, staged(fstest.MapFS{}, fstest.MapFS{}))

	if res.Status != check.StatusUnavailable {
		t.Fatalf("status = %s, want unavailable", res.Status)
	}
	if want := "this host reports no thermal sensors"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if len(res.Metrics) != 0 {
		t.Errorf("metrics = %v for a host with nothing to measure", res.Metrics)
	}
	if len(res.Trips) != 0 {
		t.Errorf("trips = %v", res.Trips)
	}
}

// A zone whose temperature cannot be read drops out rather than failing the
// walk, and the check grades what is left -- one broken sensor on a host with
// twelve should not blind the other eleven.
func TestAnUnreadableZoneDropsOutOfTheReading(t *testing.T) {
	sys := sysWithZones(zone{name: "thermal_zone0", kind: "x86_pkg_temp", milli: 45000})
	sys["class/thermal/thermal_zone1/temp"] = &fstest.MapFile{Data: []byte("banana\n")}
	sys["class/thermal/thermal_zone1/type"] = &fstest.MapFile{Data: []byte("broken\n")}

	res, _ := run(t, ThermalID, staged(fstest.MapFS{}, sys))

	if res.Status != check.StatusOK {
		t.Fatalf("status = %s, want ok: %+v", res.Status, res)
	}
	if want := "1 thermal zone, hottest is x86_pkg_temp at 45.0C"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
}

// A zone with no type file is named for its directory, which is the only label it
// has. Reported rather than dropped: an unlabelled sensor at 90C is still 90C.
func TestAZoneWithNoTypeIsLabelledByItsDirectory(t *testing.T) {
	res, _ := run(t, ThermalID, staged(fstest.MapFS{}, sysWithZones(zone{name: "thermal_zone0", milli: 51000})))

	if want := "1 thermal zone, hottest is thermal_zone0 at 51.0C"; res.Summary != want {
		t.Errorf("summary = %q, want %q", res.Summary, want)
	}
	if got := metricNamed(t, res, TempMetric).Labels["type"]; got != "thermal_zone0" {
		t.Errorf("type label = %q", got)
	}
}

// brokenGlob is a /sys whose listing itself fails. Rare -- fs.Glob only errors on
// a malformed pattern or a filesystem that reports one -- and the reason it is
// worth a test is that the alternative answer is "no sensors", which is also what
// a VM reports. A host whose sysfs will not list is not a VM.
type brokenGlob struct{ err error }

func (b brokenGlob) Open(string) (fs.File, error)  { return nil, b.err }
func (b brokenGlob) Glob(string) ([]string, error) { return nil, b.err }

func TestASysfsThatWillNotListIsAnErrorNotAnAbsenceOfSensors(t *testing.T) {
	res, _ := run(t, ThermalID, staged(fstest.MapFS{}, brokenGlob{err: errors.New("input/output error")}))

	if res.Status != check.StatusError {
		t.Fatalf("status = %s, want error -- unavailable would read as a VM", res.Status)
	}
	if !contains(res.Summary, coresystem.SysThermal) {
		t.Errorf("summary = %q, want the directory named", res.Summary)
	}
	if !contains(res.Error, "input/output error") {
		t.Errorf("error = %q, want the filesystem's own words", res.Error)
	}
}

// data[] carries every zone, not only the one that decided the verdict: an
// operator reading a crit wants to know whether the whole box is hot or one core is.
func TestThermalDataCarriesEveryZone(t *testing.T) {
	res, _ := run(t, ThermalID, staged(fstest.MapFS{}, warmHost()))

	zones, ok := res.Data.([]coresystem.ThermalZone)
	if !ok {
		t.Fatalf("data is %T, want []coresystem.ThermalZone", res.Data)
	}
	if len(zones) != 2 {
		t.Errorf("data carries %d zones, want 2", len(zones))
	}
}
