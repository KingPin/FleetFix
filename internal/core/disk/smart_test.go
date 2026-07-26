package disk

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
	"github.com/google/go-cmp/cmp"
)

// Fixture expectations come from
//
//	python tools/oracle/py_oracle.py --case smartctl.<name>
//
// and the synthetic ones from feeding the literal to the matching smart.py
// function. Re-run Python to change one; do not adjust it until Go passes.

func TestParseHealthFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    string
		ok      bool
	}{
		{name: "passed", fixture: "smartctl/sata_passed.txt", want: "PASSED", ok: true},
		{name: "failed", fixture: "smartctl/health_failed.txt", want: "FAILED!", ok: true},
		// A capture with no health line at all -- the case ParseHealth's second
		// return exists for. The full NVMe capture is not it: `smartctl -a`
		// prints the verdict above the NVMe log, so both parsers read the same
		// file, which is why the manifest has sata_passed#health.
		{name: "absent", fixture: "smartctl/nvme_comma_separated_ints.txt", want: "", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseHealth(fixture.Text(t, tt.fixture))
			if ok != tt.ok {
				t.Fatalf("ParseHealth(%s) found=%t, want %t", tt.fixture, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("ParseHealth(%s) = %q, want %q", tt.fixture, got, tt.want)
			}
		})
	}
}

func TestParseHealthText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{
			name: "verdict keeps its punctuation",
			in:   "SMART overall-health self-assessment test result: FAILED!",
			want: "FAILED!",
			ok:   true,
		},
		{
			// The pattern is case-insensitive in v1, and smartctl's own wording
			// has changed case across versions.
			name: "label case is ignored",
			in:   "smart Overall-Health Self-Assessment TEST RESULT: PASSED",
			want: "PASSED",
			ok:   true,
		},
		{
			// \s* after the colon, so no space at all still matches.
			name: "no space after the colon",
			in:   "SMART overall-health self-assessment test result:PASSED",
			want: "PASSED",
			ok:   true,
		},
		{
			// \s* can cross a newline: the verdict is the next non-space run
			// wherever it is. Faithful to the Python, and the reason a wrapped
			// line does not read as "no verdict".
			name: "verdict on the following line",
			in:   "SMART overall-health self-assessment test result:\n   PASSED\n",
			want: "PASSED",
			ok:   true,
		},
		{
			// \S+ stops at the first space, so a multi-word verdict truncates.
			name: "only the first word of the verdict",
			in:   "SMART overall-health self-assessment test result: NOT PASSED",
			want: "NOT",
			ok:   true,
		},
		{
			name: "not the last line wins",
			in: "SMART overall-health self-assessment test result: PASSED\n" +
				"SMART overall-health self-assessment test result: FAILED!\n",
			want: "PASSED",
			ok:   true,
		},
		{
			name: "no health line",
			in:   "smartctl 7.3 2022-02-28 r5338\nDevice does not support SMART\n",
			ok:   false,
		},
		{
			// The verdict group is \S+, which cannot match empty, so a label with
			// nothing after it reads as no verdict rather than as "".
			name: "colon with nothing after it",
			in:   "SMART overall-health self-assessment test result:",
			ok:   false,
		},
		{name: "empty", in: "", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseHealth(tt.in)
			if ok != tt.ok {
				t.Fatalf("ParseHealth(%q) found=%t, want %t", tt.in, ok, tt.ok)
			}
			if got != tt.want {
				t.Errorf("ParseHealth(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSATAAttributesFixture(t *testing.T) {
	want := map[string]int64{
		"reallocated_sectors":    3,
		"power_on_hours":         1234,
		"reported_uncorrect":     0,
		"current_pending_sector": 0,
		"ssd_wear_indicator":     12,
	}
	got := ParseSATAAttributes(fixture.Text(t, "smartctl/sata_passed.txt"))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseSATAAttributes(sata_passed.txt) mismatch (-want +got):\n%s", diff)
	}
}

func TestParseSATAAttributesRows(t *testing.T) {
	const header = "ID# ATTRIBUTE_NAME          FLAG     VALUE WORST THRESH TYPE      UPDATED  WHEN_FAILED RAW_VALUE\n"

	tests := []struct {
		name string
		in   string
		want map[string]int64
	}{
		{
			// The header row itself must not parse: "ID#" is not a digit run.
			name: "header alone",
			in:   header,
			want: map[string]int64{},
		},
		{
			name: "uninteresting ids are dropped",
			in:   header + "  1 Raw_Read_Error_Rate     0x000f   100   100   006    Pre-fail  Always       -       0\n",
			want: map[string]int64{},
		},
		{
			// Power_On_Hours raw values carry a suffix on plenty of drives, and
			// the leading integer run is what v1 takes.
			name: "raw value with a suffix",
			in:   header + "  9 Power_On_Hours          0x0032   099   099   000    Old_age   Always       -       12345h+0m+0.000s\n",
			want: map[string]int64{"power_on_hours": 12345},
		},
		{
			// Seagate's raw for id 5 is two numbers; the second is in parens and
			// the regex has already stopped at the space.
			name: "raw value with a parenthesised tail",
			in:   header + "  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       8 (2 3)\n",
			want: map[string]int64{"reallocated_sectors": 8},
		},
		{
			// \S+ for the raw column, so a non-numeric raw matches the row and
			// then fails the leading-digit rule. Dropped, not recorded as zero.
			name: "non-numeric raw value",
			in:   header + "  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       -\n",
			want: map[string]int64{},
		},
		{
			name: "when_failed carries a value",
			in:   header + "197 Current_Pending_Sector  0x0032   100   100   000    Old_age   Always   FAILING_NOW   4\n",
			want: map[string]int64{"current_pending_sector": 4},
		},
		{
			// Three numeric columns are required between the flag and TYPE, so a
			// row missing WORST does not match at all.
			name: "short row does not match",
			in:   header + "  5 Reallocated_Sector_Ct   0x0033   100   010    Pre-fail  Always       -       3\n",
			want: map[string]int64{},
		},
		{
			// A row is matched with .match, so leading indentation is fine and
			// trailing junk after the raw column is ignored.
			name: "deep indentation",
			in:   header + "      233 Media_Wearout_Indicator 0x0032   088   088   000    Old_age   Always       -       12  \n",
			want: map[string]int64{"ssd_wear_indicator": 12},
		},
		{
			name: "last row for an id wins",
			in: header +
				"  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       3\n" +
				"  5 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       9\n",
			want: map[string]int64{"reallocated_sectors": 9},
		},
		{
			// The id column is read as an integer, so a padded id is the same id.
			name: "zero-padded id",
			in:   header + "005 Reallocated_Sector_Ct   0x0033   100   100   010    Pre-fail  Always       -       3\n",
			want: map[string]int64{"reallocated_sectors": 3},
		},
		{
			name: "empty input",
			in:   "",
			want: map[string]int64{},
		},
		{
			// Every line ending splitlines knows, since a capture from a Windows
			// build of smartctl would arrive with \r\n. The \r lands in the
			// trailing \S+ of the raw column and the leading digits still read.
			name: "crlf line endings",
			in:   header + "  9 Power_On_Hours          0x0032   099   099   000    Old_age   Always       -       7\r\n",
			want: map[string]int64{"power_on_hours": 7},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSATAAttributes(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseSATAAttributes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// An id past int64 is dropped rather than misread. Python computes the big
// integer, misses the interesting-ids dict, and drops the row too -- same
// outcome by a different route, which is why this is a test and not a
// known-divergence entry.
func TestParseSATAAttributesHugeIDIsDropped(t *testing.T) {
	const row = "99999999999999999999 Reallocated_Sector_Ct 0x0033 100 100 010 Pre-fail Always - 3\n"
	if got := ParseSATAAttributes(row); len(got) != 0 {
		t.Errorf("ParseSATAAttributes(huge id) = %v, want no attributes", got)
	}
}

// A raw value past int64 is the documented departure: Python reports the number,
// this reports nothing. Absent reads as "not reported", which is honest; a
// clamped 2^63-1 would read as a measurement.
func TestParseSATAAttributesHugeRawIsDropped(t *testing.T) {
	const row = "  9 Power_On_Hours 0x0032 099 099 000 Old_age Always - 99999999999999999999\n"
	if got := ParseSATAAttributes(row); len(got) != 0 {
		t.Errorf("ParseSATAAttributes(huge raw) = %v, want no attributes", got)
	}
}

func TestParseNVMeAttributesFixtures(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    map[string]int64
	}{
		{
			name:    "passed",
			fixture: "smartctl/nvme_passed.txt",
			want: map[string]int64{
				"available_spare":                 100,
				"available_spare_threshold":       10,
				"percentage_used":                 3,
				"media_and_data_integrity_errors": 0,
			},
		},
		{
			name:    "comma separated ints",
			fixture: "smartctl/nvme_comma_separated_ints.txt",
			want:    map[string]int64{"media_and_data_integrity_errors": 12345},
		},
		{
			// A SATA capture has none of the NVMe labels, so every attribute is
			// absent rather than zero.
			name:    "sata output has no nvme attributes",
			fixture: "smartctl/sata_passed.txt",
			want:    map[string]int64{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseNVMeAttributes(fixture.Text(t, tt.fixture))
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseNVMeAttributes(%s) mismatch (-want +got):\n%s", tt.fixture, diff)
			}
		})
	}
}

func TestParseNVMeAttributesText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want map[string]int64
	}{
		{
			// The two spare labels overlap, and the colon is what separates
			// them: "Available Spare:" is not a prefix of "Available Spare
			// Threshold:", so the threshold line reads as one attribute, not two.
			name: "threshold line does not satisfy the spare pattern",
			in:   "Available Spare Threshold:      10%\n",
			want: map[string]int64{"available_spare_threshold": 10},
		},
		{
			name: "both spare lines",
			in:   "Available Spare:                    100%\nAvailable Spare Threshold:          10%\n",
			want: map[string]int64{"available_spare": 100, "available_spare_threshold": 10},
		},
		{
			// Multiline anchoring with leading indentation, which is how the
			// labels appear inside smartctl's SMART/Health section.
			name: "indented labels",
			in:   "  Percentage Used:         3%\n",
			want: map[string]int64{"percentage_used": 3},
		},
		{
			// The percentage patterns end in \s*%, so a value with no percent
			// sign does not match.
			name: "percentage without its sign",
			in:   "Percentage Used:         3\n",
			want: map[string]int64{},
		},
		{
			// Integrity errors have no % suffix and nothing anchoring the end of
			// the value, so a trailing unit is simply left behind.
			name: "integrity errors with a trailing word",
			in:   "Media and Data Integrity Errors:    4 errors\n",
			want: map[string]int64{"media_and_data_integrity_errors": 4},
		},
		{
			name: "separators anywhere in the integrity count",
			in:   "Media and Data Integrity Errors:    1,2,3\n",
			want: map[string]int64{"media_and_data_integrity_errors": 123},
		},
		{
			name: "first occurrence wins",
			in:   "Percentage Used:  3%\nPercentage Used:  9%\n",
			want: map[string]int64{"percentage_used": 3},
		},
		{
			// The label must start the line, so a prose mention is not a reading.
			name: "label mid-line is not a match",
			in:   "note: Percentage Used:  3%\n",
			want: map[string]int64{},
		},
		{
			name: "empty input",
			in:   "",
			want: map[string]int64{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseNVMeAttributes(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseNVMeAttributes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// The one deliberate behavioural difference in this file, pinned so it cannot
// change by accident: [\d,]+ can capture a run with no digits in it, and Python
// then calls int("") and raises ValueError -- losing the whole SMART report for
// every drive on the host, not just the attribute. Dropping the attribute is the
// chosen behaviour. No captured smartctl output produces this, so the harness
// never reaches it and it needs no known_divergences entry.
func TestParseNVMeAttributesCommaOnlyCountIsDropped(t *testing.T) {
	got := ParseNVMeAttributes("Media and Data Integrity Errors:    ,,,\nPercentage Used:  3%\n")
	want := map[string]int64{"percentage_used": 3}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseNVMeAttributes(comma-only count) mismatch (-want +got):\n%s", diff)
	}
}

// A percentage past int64 is the same magnitude departure as the SATA raw value.
func TestParseNVMeAttributesHugeValueIsDropped(t *testing.T) {
	got := ParseNVMeAttributes("Percentage Used:  99999999999999999999%\n")
	if len(got) != 0 {
		t.Errorf("ParseNVMeAttributes(huge value) = %v, want no attributes", got)
	}
}

// parse_sata_attributes is on the plan's fuzz list: nine whitespace-separated
// columns matched by a single pattern is the highest-entropy shape in this
// package. The invariants are the ones a caller relies on -- only known keys, no
// negatives, and no panic on arbitrary bytes.
func FuzzParseSATAAttributes(f *testing.F) {
	f.Add(fixture.Text(f, "smartctl/sata_passed.txt"))
	f.Add("  5 A 0x0 1 1 1 P A - 3\n")
	f.Add("\x00\r\n  9 x y 1 2 3 a b c ١٢٣\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		for k, v := range ParseSATAAttributes(s) {
			if !known(k) {
				t.Fatalf("ParseSATAAttributes(%q) invented the key %q", s, k)
			}
			if v < 0 {
				t.Fatalf("ParseSATAAttributes(%q)[%s] = %d; the raw column has no sign", s, k, v)
			}
		}
	})
}

func FuzzParseNVMeAttributes(f *testing.F) {
	f.Add(fixture.Text(f, "smartctl/nvme_passed.txt"))
	f.Add("Percentage Used: 3%\n")
	f.Add("Media and Data Integrity Errors: ,,,\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		for k, v := range ParseNVMeAttributes(s) {
			if !known(k) {
				t.Fatalf("ParseNVMeAttributes(%q) invented the key %q", s, k)
			}
			if v < 0 {
				t.Fatalf("ParseNVMeAttributes(%q)[%s] = %d; none of these fields have a sign", s, k, v)
			}
		}
	})
}

// The verdict is one non-space run, whatever the input was.
func FuzzParseHealth(f *testing.F) {
	f.Add(fixture.Text(f, "smartctl/sata_passed.txt"))
	f.Add("SMART overall-health self-assessment test result: PASSED")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		got, ok := ParseHealth(s)
		if !ok {
			if got != "" {
				t.Fatalf("ParseHealth(%q) returned %q with found=false", s, got)
			}
			return
		}
		if got == "" {
			t.Fatalf("ParseHealth(%q) returned an empty verdict with found=true", s)
		}
		if strings.ContainsFunc(got, pytext.IsSpace) {
			t.Fatalf("ParseHealth(%q) = %q, which contains whitespace", s, got)
		}
	})
}

func known(k string) bool {
	for _, name := range sataInterestingIDs {
		if k == name {
			return true
		}
	}
	switch k {
	case "percentage_used", "available_spare", "available_spare_threshold",
		"media_and_data_integrity_errors":
		return true
	}
	return false
}
