package system

import (
	"io/fs"
	"path"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/hostfs"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// SysThermal is where ReadZones looks under a Host's Sys root.
const SysThermal = "class/thermal"

// ThermalZone is one kernel thermal_zone and the temperature it currently
// reports.
type ThermalZone struct {
	// Name is the sysfs directory, "thermal_zone0". Stable across a boot and
	// otherwise meaningless -- the zone numbering is whatever order the drivers
	// registered in.
	Name string `json:"name"`
	// Type is what the driver calls the sensor: "x86_pkg_temp", "acpitz",
	// "cpu-thermal". It is the only human-readable label a zone has, but it is
	// not unique -- a multi-socket host reports several "x86_pkg_temp" -- so it
	// identifies the kind of sensor, not the sensor.
	Type string `json:"type"`
	// TempC is degrees Celsius. The kernel reports millidegrees, so a zone can
	// legitimately land on a fractional value.
	TempC float64 `json:"temp_c"`
}

// ReadZones lists every thermal zone with a temperature that can be read.
//
// Order is lexicographic on the directory name, which puts thermal_zone10 before
// thermal_zone2. Numeric order would read better in a table and is deliberately
// not what this returns: v1 sorts the glob the same way, the ordering is what
// Hottest resolves a tie by, and a table that wants numeric order can sort for
// itself.
//
// One departure, the same one ReadMeminfo has: a millidegree reading past int64
// skips the zone, where Python's arbitrary-precision int carries it through. Not
// reachable from a kernel that reports millidegrees in a long.
func ReadZones(fsys fs.FS, dir string) ([]ThermalZone, error) {
	// No stat on dir first, unlike v1's `if not root.exists(): return []`. Glob
	// reports no match for a root that is missing, is a file, or cannot be listed,
	// which is the same empty answer v1 arrives at by all three routes -- and one
	// less race on a tree whose entries appear and vanish with the hardware.
	entries, err := fs.Glob(fsys, path.Join(dir, "thermal_zone*"))
	if err != nil {
		return nil, err
	}

	zones := []ThermalZone{}
	for _, entry := range entries {
		name := path.Base(entry)

		raw, err := hostfs.ReadFile(fsys, path.Join(entry, "temp"))
		if err != nil {
			// Every reason a temperature cannot be had drops the zone rather than
			// failing the walk, matching v1's exists() check plus its OSError and
			// ValueError handler: an entry with no temp file at all, a regular file
			// where a directory was expected, EACCES, and the ENODEV a disabled zone
			// returns.
			continue
		}
		// str.strip() before int(), not int() alone. The two disagree: str.strip
		// removes U+001C..U+001F and int()'s own leading-whitespace skip does not,
		// so "\x1c7000" is 7000 to v1 and would be a ValueError without the trim.
		milli, err := pytext.Int(strings.TrimFunc(raw, pytext.IsSpace))
		if err != nil {
			continue
		}

		// Read outside that tolerance on purpose. v1 wraps only the temperature in
		// its try, so a type file that exists and then will not read propagates out
		// of read_zones and loses the zones already collected. Asymmetric, and
		// arguably a bug, but it is the answer v1 gives and the alternative --
		// swallowing it -- would report a host with unreadable sysfs as a host with
		// no sensors.
		//
		// Absent is not a failure: the fallback is the directory name, so an entry
		// with a temperature and no label still reports.
		zoneType := name
		if label, err := hostfs.ReadFile(fsys, path.Join(entry, "type")); err == nil {
			zoneType = strings.TrimFunc(label, pytext.IsSpace)
		} else if !hostfs.IsAbsent(err) {
			return nil, err
		}

		zones = append(zones, ThermalZone{
			Name:  name,
			Type:  zoneType,
			TempC: float64(milli) / 1000.0,
		})
	}
	return zones, nil
}

// Hottest returns the warmest zone, and false on a host with no thermal sensors
// -- most VMs, and every container.
//
// On a tie the first zone wins, which is Python's max() keeping its incumbent and
// therefore ReadZones' lexicographic order. It matters more than it looks: a
// multi-socket host reports several zones of the same type sitting at the same
// temperature for long stretches, and a caller that renders the winner's name
// should not see it flap.
//
// Takes the zones rather than reading them, where v1's signature defaults to
// calling read_zones() itself. The caller already has them -- the dashboard
// renders the whole list next to the hottest one -- and the default silently
// turned one read into two.
func Hottest(zones []ThermalZone) (ThermalZone, bool) {
	if len(zones) == 0 {
		return ThermalZone{}, false
	}
	best := zones[0]
	for _, z := range zones[1:] {
		if z.TempC > best.TempC {
			best = z
		}
	}
	return best, true
}
