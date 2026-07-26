package disk

import (
	"regexp"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// The handful of SMART attributes that actually correlate with imminent failure,
// keyed by id rather than by name: vendors rename them, and a Seagate
// reallocation count is not spelled the same as a WD one.
var sataInterestingIDs = map[int64]string{
	5:   "reallocated_sectors",
	9:   "power_on_hours",
	187: "reported_uncorrect",
	197: "current_pending_sector",
	233: "ssd_wear_indicator",
}

// The pattern fragments, with \s, \S and \d replaced by the pytext classes so
// they match what Python's re matches rather than the ASCII subset Go's
// shorthands cover.
const (
	optSpaces = pytext.Space + `*`
	spaces    = pytext.Space + `+`
	nonSpace  = pytext.NotSpace + `+`
	digits    = pytext.Digit + `+`
	// Thousands-separated digits: "12,345".
	groupedDigits = `[\p{Nd},]+`
)

// The v1 patterns, otherwise character-for-character what smart.py has.
var (
	healthRe = regexp.MustCompile(
		`(?i)SMART overall-health self-assessment test result:` + optSpaces + `(` + nonSpace + `)`,
	)

	// A SATA attribute row:
	//
	//	"  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       0"
	//
	// Anchored at the start only, because Python matches this one with re.match.
	sataRowRe = regexp.MustCompile(`^` + optSpaces + `(` + digits + `)` +
		spaces + nonSpace + spaces + nonSpace +
		spaces + digits + spaces + digits + spaces + digits +
		spaces + nonSpace + spaces + nonSpace + spaces + nonSpace +
		spaces + `(` + nonSpace + `)`)

	// The leading integer of a raw value: it is "0" or "12345" or, for
	// Power_On_Hours, "12345h+0m+0.000s".
	leadingDigitsRe = regexp.MustCompile(`^` + digits)

	// NVMe rows are colon-separated: "Percentage Used:                    3%".
	nvmePctUsedRe          = nvmeRe(`Percentage Used:`, digits, optSpaces+`%`)
	nvmeAvailSpareRe       = nvmeRe(`Available Spare:`, digits, optSpaces+`%`)
	nvmeAvailSpareThreshRe = nvmeRe(`Available Spare Threshold:`, digits, optSpaces+`%`)
	nvmeIntegrityRe        = nvmeRe(`Media and Data Integrity Errors:`, groupedDigits, ``)
)

// nvmeRe builds one of the multiline "Label:   value" patterns. The Python
// originals are separate literals; sharing the shape here keeps the four of them
// from drifting apart, which is how the label of one ends up on the value rule
// of another.
func nvmeRe(label, value, suffix string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^` + optSpaces + regexp.QuoteMeta(label) + spaces + `(` + value + `)` + suffix)
}

// ParseHealth pulls the PASSED/FAILED verdict out of `smartctl -H` output.
//
// The second return is false when the line is absent, which is not the same as a
// drive that failed: smartctl omits it entirely for a device it could not open,
// and reporting that as "not PASSED" would raise an alert about a missing cable.
func ParseHealth(text string) (string, bool) {
	m := healthRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ParseSATAAttributes pulls the SATA attributes worth watching out of
// `smartctl -A` output.
//
// Rows are looked up by the id/name pair rather than by column position because
// smartctl's column layout varies by drive and firmware; an attribute whose row
// does not match the shape is left out rather than guessed at.
func ParseSATAAttributes(text string) map[string]int64 {
	out := map[string]int64{}
	for _, line := range pytext.SplitLines(text) {
		m := sataRowRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		attrID, err := pytext.Int(m[1])
		if err != nil {
			// An id too long for an int64 cannot be one of the five, so Python
			// reaches the same skip by way of a dictionary miss.
			continue
		}
		name, interesting := sataInterestingIDs[attrID]
		if !interesting {
			continue
		}
		rawDigits := leadingDigitsRe.FindString(m[2])
		if rawDigits == "" {
			continue
		}
		v, err := pytext.Int(rawDigits)
		if err != nil {
			// Only reachable past 2^63-1 hours or reallocated sectors, where
			// Python would report the true number and this reports nothing.
			// Absent reads as "not reported"; a clamped value would read as a
			// real measurement.
			continue
		}
		out[name] = v
	}
	return out
}

// ParseNVMeAttributes pulls the NVMe health attributes out of `smartctl -A`
// output.
func ParseNVMeAttributes(text string) map[string]int64 {
	out := map[string]int64{}
	for _, f := range []struct {
		key string
		re  *regexp.Regexp
	}{
		{"percentage_used", nvmePctUsedRe},
		{"available_spare", nvmeAvailSpareRe},
		{"available_spare_threshold", nvmeAvailSpareThreshRe},
		{"media_and_data_integrity_errors", nvmeIntegrityRe},
	} {
		m := f.re.FindStringSubmatch(text)
		if m == nil {
			continue
		}
		// Thousands separators are dropped wherever they fall, so "1,,2" is 12.
		// Only the integrity-error pattern can produce one, and stripping
		// unconditionally is what Python does to that group.
		v, err := pytext.Int(stripCommas(m[1]))
		if err != nil {
			// Python raises ValueError here for a value that is all separators
			// ("Media and Data Integrity Errors:  ,,,"), taking the whole SMART
			// scan down with it. Dropping the attribute is the deliberate
			// difference: no captured output contains it, and a malformed field
			// on one drive should not cost the report for every other drive.
			continue
		}
		out[f.key] = v
	}
	return out
}

func stripCommas(s string) string {
	out := make([]byte, 0, len(s))
	for i := range len(s) {
		if s[i] != ',' {
			out = append(out, s[i])
		}
	}
	return string(out)
}
