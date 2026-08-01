// Package disk parses the output of the filesystem and SMART tools.
//
// Port of src/fleetfix/modules/disk/. Parsing only: nothing here runs a command or
// touches a real filesystem, which is what lets the differential harness feed the
// same captured bytes to this and to the Python original and compare the results.
//
// The severity constants that lived beside these parsers in v1 (WARN_PCT,
// CRITICAL_PCT) are deliberately absent. They move to thresholds.yml at M3, graded
// by one shared grader for all three front doors, and keeping a second copy here
// would be the thing that later disagrees with it.
package disk

import (
	"math"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Usage is one row of `df -P -k`: a real mount and how full it is.
//
// Sizes stay in the 1024-byte blocks df reports rather than being normalised to
// bytes. The port is byte-compatible with v1's wire output first and tidy second,
// and total_kb is what the Python dataclass calls it.
type Usage struct {
	Filesystem string `json:"filesystem"`
	Mount      string `json:"mount"`
	TotalKB    int64  `json:"total_kb"`
	UsedKB     int64  `json:"used_kb"`
	AvailKB    int64  `json:"avail_kb"`
	UsedPct    int64  `json:"used_pct"`
}

// skipLinePrefixes are dropped before any parsing.
//
// Matched against the whole line, not the filesystem field, because that is what
// Python's str.startswith(tuple) does here. The difference is observable: a mount
// point named /dev/loop-something on a row whose filesystem field is /dev/sda1
// would not be skipped either way, but a filesystem field that merely *contains*
// one of these is not skipped by either implementation, and narrowing the match to
// the first field would change that.
var skipLinePrefixes = []string{"tmpfs", "devtmpfs", "squashfs", "overlay", "udev", "/dev/loop"}

const headerPrefix = "Filesystem"

// ParseDF parses `df -P -k` output, skipping pseudo-filesystems and mounts with no
// usable capacity.
//
// POSIX mode (-P) guarantees six columns -- Filesystem, 1024-blocks, Used,
// Available, Capacity, Mounted on -- and keeps the row on one line however long
// the device name is. The split is capped at five so a mount point containing
// spaces survives as one field; anything else would truncate `/mnt/with space`.
func ParseDF(text string) []Usage {
	// Non-nil so the JSON is [] and not null. Python returns a list, and an
	// absent-versus-empty difference is a divergence the harness would report on
	// every host with no mounted filesystems.
	out := []Usage{}
	for _, line := range pytext.SplitLines(text) {
		if line == "" || strings.HasPrefix(line, headerPrefix) {
			continue
		}
		if hasAnyPrefix(line, skipLinePrefixes) {
			continue
		}
		parts := pytext.SplitN(line, 5)
		if len(parts) < 6 {
			continue
		}
		total, errTotal := pytext.Int(parts[1])
		used, errUsed := pytext.Int(parts[2])
		avail, errAvail := pytext.Int(parts[3])
		if errTotal != nil || errUsed != nil || errAvail != nil {
			// df prints '-' for a filesystem with no usable accounting.
			continue
		}
		if total == 0 {
			// A zero-capacity mount carries no signal; /boot/efi on a fresh
			// install is the common one.
			continue
		}
		out = append(out, Usage{
			Filesystem: parts[0],
			Mount:      parts[5],
			TotalKB:    total,
			UsedKB:     used,
			AvailKB:    avail,
			UsedPct:    usedPercent(parts[4], used, total),
		})
	}
	return out
}

// usedPercent reads df's Capacity column, falling back to computing it.
//
// The fallback exists because df prints '-' for the capacity of a filesystem it
// cannot account for while still reporting block counts, which is the
// usage_missing_capacity fixture.
//
// TrimRight, not TrimSuffix: Python's rstrip("%") removes every trailing '%',
// so "62%%" reads as 62 in both implementations.
func usedPercent(field string, used, total int64) int64 {
	if n, err := pytext.Int(strings.TrimRight(field, "%")); err == nil {
		return n
	}
	if total == 0 {
		return 0
	}
	// RoundToEven, not Round. Python's round() on a float is round-half-to-even,
	// so round(62.5) is 62 and round(63.5) is 64; math.Round would return 63 and
	// 64. Only a used/total ratio landing exactly on .5 can tell them apart, which
	// no fixture does and a fuzzer does immediately.
	return int64(math.RoundToEven(float64(used) * 100 / float64(total)))
}

// Fullest returns the mount with the highest used percentage, or false when there
// are none. The dashboard headline.
//
// Ties go to the first row, matching Python's max(), which keeps the earliest
// element when the key is equal.
func Fullest(rows []Usage) (Usage, bool) {
	if len(rows) == 0 {
		return Usage{}, false
	}
	best := 0
	for i := 1; i < len(rows); i++ {
		if rows[i].UsedPct > rows[best].UsedPct {
			best = i
		}
	}
	return rows[best], true
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
