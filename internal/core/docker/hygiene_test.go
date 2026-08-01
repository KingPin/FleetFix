package docker

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/google/go-cmp/cmp"
)

// Expectations come from src/fleetfix/modules/docker/hygiene.py under CPython
// 3.14.6, fed the same literals, except where a case is marked as a departure.

const (
	// Homoglyphs and invisibles, named so a reader can tell them apart from the
	// ASCII letters they are here to be distinguished from.
	kelvin   = "\u212a" // KELVIN SIGN, indistinguishable from K
	longS    = "\u017f" // LATIN SMALL LETTER LONG S, folds with s
	dottedI  = "\u0130" // LATIN CAPITAL LETTER I WITH DOT ABOVE
	dotlessI = "\u0131" // LATIN SMALL LETTER DOTLESS I
	nbsp     = "\u00a0" // NO-BREAK SPACE
	fileSep  = "\u001c" // FILE SEPARATOR, whitespace to Python and not to Go
	nel      = "\u0085" // NEXT LINE
	arabic5  = "\u0665" // ARABIC-INDIC DIGIT FIVE
)

func TestParseSize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"empty", "", 0},
		{"whitespace", "   ", 0},

		// The shapes docker prints.
		{"bytes", "136B", 136},
		{"gigabytes", "45.68GB", 45680000000},
		{"megabytes", "444.8MB", 444800000},
		{"zero", "0B", 0},

		// Decimal prefixes are powers of a thousand, binary ones of 1024.
		{"kb", "1KB", 1000},
		{"mb", "1MB", 1000000},
		{"gb", "1GB", 1000000000},
		{"tb", "1TB", 1000000000000},
		{"kib", "1KiB", 1024},
		{"mib", "1MiB", 1048576},
		{"gib", "1GiB", 1073741824},
		{"tib", "1TiB", 1099511627776},

		// The unit is upper-cased before the lookup, so any spelling works.
		{"lowercase kb", "1kb", 1000},
		{"lowercase kib", "1kib", 1024},
		{"lowercase tib", "5tib", 5497558138880},
		{"mixed case kib", "5kIb", 5120},
		{"bare b", "5b", 5},

		// An unlisted unit is bytes rather than a refusal, so these are all five.
		{"no unit", "5", 5},
		{"unknown unit", "5X", 5},
		{"unknown two-letter unit", "5ZB", 5},
		{"i without a prefix", "5iB", 5},
		{"doubled suffix", "5KiBB", 5},
		{"doubled b", "5BB", 5},

		// A unit with no number is not a size.
		{"unit alone", "B", 0},
		// KELVIN SIGN is not in [A-Za-z], which has no IGNORECASE here.
		{"kelvin unit", "5" + kelvin + "b", 0},
		// [0-9], not \d: an Arabic-Indic five is not a digit to this pattern.
		{"unicode digits", arabic5 + "GB", 0},
		// [0-9.] captures no sign, so the whole anchored match fails.
		{"minus", "-5GB", 0},
		{"plus", "+5GB", 0},

		// Only U+0020 is removed from the middle, and it is removed everywhere.
		{"space before the unit", "45.68 GB", 45680000000},
		{"spaces around", "  45.68 GB  ", 45680000000},
		{"tab before the unit", "45.68\tGB", 0},
		{"newline before the unit", "45.68\nGB", 0},
		{"non-breaking space before the unit", "45.68" + nbsp + "GB", 0},
		{"file separator before the unit", "45.68" + fileSep + "GB", 0},

		// str.strip() strips everything str.isspace() accepts, which is more than
		// Go's unicode.IsSpace does.
		{"non-breaking space around", nbsp + "45.68GB" + nbsp, 45680000000},
		{"file separator around", fileSep + "45.68GB" + fileSep, 45680000000},
		{"next line around", nel + "45.68GB" + nel, 45680000000},
		{"trailing newline", "45.68GB\n", 45680000000},

		// [0-9.]+ matches runs of dots that float() will not take. v1 catches the
		// ValueError and reports no size.
		{"two dots", "1.2.3GB", 0},
		{"trailing dots", "5..GB", 0},
		{"dots alone", "...GB", 0},
		{"dot alone", ".", 0},
		{"leading dot", ".5GB", 500000000},
		{"trailing dot", "1.GB", 1000000000},

		// An exponent is not in [0-9.], so 1e3KB is not a size at all.
		{"exponent", "1e3KB", 0},

		// int() truncates towards zero rather than rounding.
		{"fraction truncates", "1.9B", 1},
		{"fraction below one byte", "0.0001KB", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseSize(tt.in); got != tt.want {
				t.Errorf("ParseSize(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestParseSizeDepartsOnInputsV1CannotAnswer covers the two failure classes named
// in ParseSize's comment. v1 raises OverflowError out of int() on the first and
// reports an arbitrary-precision integer on the second; both are 0 here, and
// neither is reachable from output docker produces.
func TestParseSizeDepartsOnInputsV1CannotAnswer(t *testing.T) {
	tests := []struct {
		name string
		in   string
	}{
		{"number overflows a float64", strings.Repeat("9", 400) + "GB"},
		{"number at the top of a float64", "1" + strings.Repeat("0", 308) + "B"},
		{"product overflows an int64", "18446744073709551616B"},
		{"multiplied product overflows an int64", "18446744073709551GB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseSize(tt.in); got != 0 {
				t.Errorf("ParseSize(%q) = %d, want 0", tt.in, got)
			}
		})
	}
}

// TestParseSizeDepartureInputsAreReallyTheOverflowPath guards the cases above
// against passing for the wrong reason: each must still match the size pattern,
// so a typo that stopped the regex matching would leave them green while testing
// nothing.
func TestParseSizeDepartureInputsAreReallyTheOverflowPath(t *testing.T) {
	for _, in := range []string{
		strings.Repeat("9", 400) + "GB",
		"1" + strings.Repeat("0", 308) + "B",
		"18446744073709551616B",
		"18446744073709551GB",
	} {
		if !reSize.MatchString(in) {
			t.Errorf("the size pattern did not match %.20q..., so the overflow guard is untested", in)
		}
	}
}

func TestParseReclaimable(t *testing.T) {
	tests := []struct {
		name      string
		in        string
		wantBytes int64
		wantPct   int64
	}{
		{"empty", "", 0, 0},

		// The two shapes docker prints.
		{"size and percentage", "45.68GB (85%)", 45680000000, 85},
		{"size alone", "7.817GB", 7817000000, 0},
		{"zero of both", "0B (0%)", 0, 0},

		{"no space before the paren", "45.68GB(85%)", 45680000000, 85},
		{"space inside the size", "45.68 GB (85%)", 45680000000, 85},
		{"leading zeroes in the percentage", "45.68GB (0085%)", 45680000000, 85},
		{"surrounding whitespace", "  45.68GB (85%)  ", 45680000000, 85},
		{"surrounding non-breaking spaces", nbsp + "45.68GB (85%)" + nbsp, 45680000000, 85},
		{"trailing newline", "45.68GB (85%)\n", 45680000000, 85},
		{"trailing crlf", "45.68GB (85%)\r\n", 45680000000, 85},
		{"newline before the percentage", "45.68GB\n(85%)", 45680000000, 85},
		{"trailing newline, no percentage", "45.68GB\n", 45680000000, 0},

		// \d is Nd, and int() takes those digits, so this really is five percent.
		{"unicode percentage digits", "45.68GB (" + arabic5 + "%)", 45680000000, 5},

		// The column's own pattern allows whitespace between number and unit where
		// ParseSize allows only U+0020, so the size is unreadable and the
		// percentage is not.
		{"tab inside the size", "45.68\tGB (85%)", 0, 85},
		{"newline inside the size", "45.68\nGB (85%)", 0, 85},
		{"non-breaking space inside the size", "45.68" + nbsp + "GB (85%)", 0, 85},

		// The pattern is anchored at both ends, so anything it cannot account for
		// makes the whole column unreadable rather than partly readable.
		{"space before the percent sign", "45.68GB (85 %)", 0, 0},
		{"percentage without parens", "45.68GB 85%", 0, 0},
		{"two percentages", "45.68GB (85%) (10%)", 0, 0},
		{"trailing junk", "45.68GB (85%) extra", 0, 0},
		{"percentage alone", "(85%)", 0, 0},
		{"bare percentage", "85%", 0, 0},
		{"not a size", "abc", 0, 0},

		// \d+ needs a digit, so the optional group simply does not match and the
		// percentage reads as absent.
		{"empty parens", "45.68GB ()", 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseReclaimableBytes(tt.in); got != tt.wantBytes {
				t.Errorf("ParseReclaimableBytes(%q) = %d, want %d", tt.in, got, tt.wantBytes)
			}
			if got := ParseReclaimablePct(tt.in); got != tt.wantPct {
				t.Errorf("ParseReclaimablePct(%q) = %d, want %d", tt.in, got, tt.wantPct)
			}
		})
	}
}

// TestParseReclaimablePctDepartsOnAPercentageNoInt64Holds: v1's ints are
// arbitrary-precision, so it reports the number; docker prints two digits.
func TestParseReclaimablePctDepartsOnAPercentageNoInt64Holds(t *testing.T) {
	in := "1B (" + strings.Repeat("9", 25) + "%)"
	if got := ParseReclaimablePct(in); got != 0 {
		t.Errorf("ParseReclaimablePct(%q) = %d, want 0", in, got)
	}
	if got := ParseReclaimableBytes(in); got != 1 {
		t.Errorf("ParseReclaimableBytes(%q) = %d, want 1 -- the case is not exercising the overflow", in, got)
	}
}

func TestParseReclaimedTotalFixture(t *testing.T) {
	const name = "docker/prune_reclaimed.txt"
	if got := ParseReclaimedTotal(fixture.Text(t, name)); got != 123400000 {
		t.Errorf("ParseReclaimedTotal(%s) = %d, want 123400000", name, got)
	}
}

func TestParseReclaimedTotal(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want int64
	}{
		{"empty", "", 0},
		{"classic", "Total reclaimed space: 123.4MB", 123400000},
		{"zero", "Total reclaimed space: 0B", 0},
		{"embedded in a line", "x Total reclaimed space: 1B y", 1},
		{"newline after the colon", "Total reclaimed space:\n1B", 1},
		{"non-breaking space after the colon", "Total reclaimed space:" + nbsp + "1B", 1},
		{"space inside the size", "Total reclaimed space:  1 GB", 1000000000},
		{"a percentage trailer is not part of the size", "Total reclaimed space: 45.68GB (85%)", 45680000000},

		// re.IGNORECASE, so the label is case-insensitive.
		{"lowercase", "total reclaimed space: 1.5GB", 1500000000},
		{"uppercase and no space", "TOTAL RECLAIMED SPACE:2MB", 2000000},

		// The label is otherwise literal.
		{"no colon", "Total reclaimed space 1B", 0},
		{"no space in the label", "Totalreclaimed space: 1B", 0},
		{"tab in the label", "Total\treclaimed space: 1B", 0},

		{"no size", "Total reclaimed space:", 0},
		{"unparseable size", "Total reclaimed space: abc", 0},
		// search, not findall: the first trailer is the answer.
		{"first occurrence wins", "Total reclaimed space: 1B\nTotal reclaimed space: 2GB", 1},

		// IGNORECASE widens [A-Za-z] past ASCII, so these units are captured here
		// and then rejected by the size pattern, which has no IGNORECASE. The
		// answer is 0 rather than the 5 a narrower capture would have produced.
		{"kelvin unit", "Total reclaimed space: 5" + kelvin, 0},
		{"long s unit", "Total reclaimed space: 5" + longS, 0},
		{"dotted i unit", "Total reclaimed space: 5" + dottedI, 0},
		{"dotless i unit", "Total reclaimed space: 5" + dotlessI, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseReclaimedTotal(tt.in); got != tt.want {
				t.Errorf("ParseReclaimedTotal(%q) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

// TestReclaimedUnitClassMatchesPythonsIgnorecaseSet is the proof behind
// asciiLettersFolded: over every code point, the Go class under (?i) accepts
// exactly what Python's [A-Za-z] under re.IGNORECASE accepts.
//
// The Python side of that claim was measured the same way, by scanning the range
// against re. Kept as a scan rather than four spot checks because the failure
// being guarded against is a fold orbit neither of us thought to name.
func TestReclaimedUnitClassMatchesPythonsIgnorecaseSet(t *testing.T) {
	python := map[rune]bool{
		0x0130: true, // LATIN CAPITAL LETTER I WITH DOT ABOVE
		0x0131: true, // LATIN SMALL LETTER DOTLESS I
		0x017F: true, // LATIN SMALL LETTER LONG S
		0x212A: true, // KELVIN SIGN
	}
	for r := 'A'; r <= 'Z'; r++ {
		python[r] = true
	}
	for r := 'a'; r <= 'z'; r++ {
		python[r] = true
	}

	class := regexp.MustCompile(`(?i)\A` + asciiLettersFolded + `\z`)
	for r := rune(0); r <= 0x10FFFF; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue // not a character; string(r) would be U+FFFD
		}
		if got := class.MatchString(string(r)); got != python[r] {
			t.Fatalf("the folded unit class matches %U = %v, want %v", r, got, python[r])
		}
	}
}

func TestParseSystemDFJSONLinesFixture(t *testing.T) {
	const name = "docker/system_df_json.txt"
	want := []DfRow{
		{Type: "Images", TotalCount: 114, Active: 6, SizeBytes: 53500000000, ReclaimableBytes: 45680000000, ReclaimablePct: 85},
		{Type: "Containers", TotalCount: 7, Active: 7, SizeBytes: 136},
		{Type: "Local Volumes", TotalCount: 26, Active: 2, SizeBytes: 17710000000, ReclaimableBytes: 444800000, ReclaimablePct: 2},
		{Type: "Build Cache", TotalCount: 202, SizeBytes: 7817000000, ReclaimableBytes: 7817000000},
	}
	got := ParseSystemDFJSONLines(fixture.Text(t, name))
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("ParseSystemDFJSONLines(%s) mismatch (-want +got):\n%s", name, diff)
	}
}

func TestParseSystemDFJSONLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []DfRow
	}{
		{"empty", "", []DfRow{}},
		{
			"classic",
			`{"Type":"Images","TotalCount":"114","Active":"6","Size":"53.5GB","Reclaimable":"45.68GB (85%)"}` + "\n",
			[]DfRow{{Type: "Images", TotalCount: 114, Active: 6, SizeBytes: 53500000000, ReclaimableBytes: 45680000000, ReclaimablePct: 85}},
		},
		{
			"no reclaimable percentage",
			`{"Type":"Build Cache","TotalCount":"202","Active":"0","Size":"7.817GB","Reclaimable":"7.817GB"}` + "\n",
			[]DfRow{{Type: "Build Cache", TotalCount: 202, SizeBytes: 7817000000, ReclaimableBytes: 7817000000}},
		},

		// Every field has a default, so an object with none of them is a row.
		{"no keys", "{}\n", []DfRow{{}}},
		{"empty size", `{"Size":""}` + "\n", []DfRow{{}}},

		// Lines that are not JSON are skipped, blank ones ignored, and the rest
		// stripped first.
		{"garbage line", "not json\n" + `{"Type":"Images"}` + "\n", []DfRow{{Type: "Images"}}},
		{"blank lines", "\n" + `{"Type":"Images"}` + "\n\n", []DfRow{{Type: "Images"}}},
		{"indented", `   {"Type":"Images"}   ` + "\n", []DfRow{{Type: "Images"}}},
		{"no trailing newline", `{"Type":"Images"}`, []DfRow{{Type: "Images"}}},
		{"crlf", `{"Type":"Images"}` + "\r\n", []DfRow{{Type: "Images"}}},
		// json.loads rejects a second value on the line, so the line is dropped.
		{"trailing data", `{"Type":"Images"} {"Type":"Containers"}` + "\n", []DfRow{}},
		// Last one wins, in both decoders.
		{"duplicate keys", `{"Type":"a","Type":"b"}` + "\n", []DfRow{{Type: "b"}}},

		// int(): a count arrives as a string from docker, but int() takes rather
		// more than that.
		{"numeric counts", `{"TotalCount":114,"Active":6}` + "\n", []DfRow{{TotalCount: 114, Active: 6}}},
		{"fractional counts truncate towards zero", `{"TotalCount":3.9,"Active":-3.9}` + "\n", []DfRow{{TotalCount: 3, Active: -3}}},
		{"exponent count", `{"TotalCount":1e3}` + "\n", []DfRow{{TotalCount: 1000}}},
		{"bool counts", `{"TotalCount":true,"Active":false}` + "\n", []DfRow{{TotalCount: 1}}},
		{"null counts", `{"TotalCount":null,"Active":null}` + "\n", []DfRow{{}}},
		{"container count", `{"TotalCount":[1]}` + "\n", []DfRow{{}}},
		{"count with surrounding whitespace", `{"TotalCount":"  7  "}` + "\n", []DfRow{{TotalCount: 7}}},
		{"count with an underscore", `{"TotalCount":"1_0"}` + "\n", []DfRow{{TotalCount: 10}}},
		{"count in unicode digits", `{"TotalCount":"` + arabic5 + `"}` + "\n", []DfRow{{TotalCount: 5}}},
		{"unparseable count", `{"TotalCount":"abc"}` + "\n", []DfRow{{}}},
		// int("3.9") raises where int(3.9) does not.
		{"fractional count as a string", `{"TotalCount":"3.9"}` + "\n", []DfRow{{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSystemDFJSONLines(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseSystemDFJSONLines(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// TestParseSystemDFJSONLinesDepartsOnInputsV1CannotAnswer covers the cases named
// in ParseSystemDFJSONLines' comment: five uncaught exceptions and two fields v1
// would fill with something its own type annotation forbids. Docker emits none of
// these shapes.
func TestParseSystemDFJSONLinesDepartsOnInputsV1CannotAnswer(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []DfRow
	}{
		// v1: AttributeError from .get on a non-mapping.
		{"scalar line", "5\n", []DfRow{}},
		{"string line", `"x"` + "\n", []DfRow{}},
		{"null line", "null\n", []DfRow{}},
		{"array line", "[1]\n", []DfRow{}},

		// v1: AttributeError from .strip, and TypeError from re.match.
		{"non-string size", `{"Size":5}` + "\n", []DfRow{{}}},
		{"non-string reclaimable", `{"Reclaimable":5}` + "\n", []DfRow{{}}},
		// Falsy is the exception: `value or ""` catches those before v1 crashes.
		{"zero size", `{"Size":0,"Reclaimable":null}` + "\n", []DfRow{{}}},

		// v1: OverflowError from int(inf).
		{"infinite count", `{"TotalCount":1e400}` + "\n", []DfRow{{}}},

		// v1: the number, exactly, in a field no int64 can hold.
		{"count past an int64", `{"TotalCount":"18446744073709551616"}` + "\n", []DfRow{{}}},

		// v1: stored verbatim in a field it annotates str. An absent key is the
		// only one that defaults to "" there.
		{"non-string type", `{"Type":5}` + "\n", []DfRow{{}}},
		{"null type", `{"Type":null}` + "\n", []DfRow{{}}},

		// v1: NaN is a float literal to Python's decoder, and int(nan) raises a
		// ValueError _int_or_zero catches. Go's decoder rejects the line.
		{"nan count", `{"TotalCount":NaN}` + "\n", []DfRow{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseSystemDFJSONLines(tt.in)
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("ParseSystemDFJSONLines(%q) mismatch (-want +got):\n%s", tt.in, diff)
			}
		})
	}
}

// TestIntOrZero reaches the coercions directly, because a DfRow only ever shows
// two of the fields they feed and the JSON literal that produces a json.Number is
// not the only way one can arrive.
func TestIntOrZero(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want int64
	}{
		{"absent", nil, 0},
		{"string", "114", 114},
		{"string with whitespace", " 7 ", 7},
		{"unparseable string", "abc", 0},
		{"integer literal", json.Number("114"), 114},
		{"negative integer literal", json.Number("-114"), -114},
		{"fractional literal", json.Number("3.9"), 3},
		{"negative fractional literal", json.Number("-3.9"), -3},
		{"exponent literal", json.Number("1e3"), 1000},
		{"overflowing literal", json.Number("1e400"), 0},
		{"malformed literal", json.Number("nonsense"), 0},
		{"true", true, 1},
		{"false", false, 0},
		{"float", 3.9, 0},
		{"map", map[string]any{}, 0},
		{"slice", []any{}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := intOrZero(tt.in); got != tt.want {
				t.Errorf("intOrZero(%#v) = %d, want %d", tt.in, got, tt.want)
			}
		})
	}
}

func TestStringField(t *testing.T) {
	tests := []struct {
		name string
		in   any
		want string
	}{
		{"absent", nil, ""},
		{"string", "45.68GB", "45.68GB"},
		{"number", json.Number("5"), ""},
		{"bool", true, ""},
		{"map", map[string]any{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stringField(tt.in); got != tt.want {
				t.Errorf("stringField(%#v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestParseSystemDFJSONLinesReturnsEmptyNotNil(t *testing.T) {
	if got := ParseSystemDFJSONLines(""); got == nil {
		t.Error(`ParseSystemDFJSONLines("") = nil, want an empty slice`)
	}
}

func FuzzParseSize(f *testing.F) {
	for _, seed := range []string{
		"", "136B", "45.68GB", "  45.68 GB  ", "1KiB", "5X", "1.2.3GB", ".5GB",
		strings.Repeat("9", 400) + "GB",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		// [0-9.]+ captures no sign and every multiplier is positive, so a negative
		// size is not a reading either implementation can produce -- and the
		// overflow guard is what keeps an int64 wrap from inventing one.
		if got := ParseSize(in); got < 0 {
			t.Fatalf("ParseSize(%q) = %d, want a non-negative size", in, got)
		}
	})
}

func FuzzParseSystemDFJSONLines(f *testing.F) {
	for _, seed := range []string{
		"",
		`{"Type":"Images","TotalCount":"114","Active":"6","Size":"53.5GB","Reclaimable":"45.68GB (85%)"}` + "\n",
		"not json\n{}\n",
		`{"TotalCount":3.9}` + "\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, in string) {
		for _, row := range ParseSystemDFJSONLines(in) {
			if row.SizeBytes < 0 || row.ReclaimableBytes < 0 || row.ReclaimablePct < 0 {
				t.Fatalf("ParseSystemDFJSONLines(%q) produced a negative size: %#v", in, row)
			}
			// The type is copied out of a JSON string, so it cannot hold a line
			// break -- a row that claimed otherwise would mean the line splitting
			// and the decoding had come apart.
			if strings.ContainsAny(row.Type, "\n\r") {
				t.Fatalf("ParseSystemDFJSONLines(%q) produced a type holding a line break: %#v", in, row)
			}
		}
	})
}
