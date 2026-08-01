package updater

import (
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// assetName is the amd64 asset name, frozen forever because v1.6.0 installs look
// for exactly this string. Most cases below are a digest, a separator and this.
const assetName = "fleetfix-linux-x86_64"

// The runes that make the lowercasing observable. Named rather than written inline
// because two of them are indistinguishable from an ASCII letter in most fonts,
// which is exactly how a wrong expectation would survive review.
const (
	kelvin   = "\u212A" // KELVIN SIGN: lowercases to ASCII "k"
	dotlessI = "ı"      // LATIN SMALL LETTER DOTLESS I: already lowercase
	dottedI  = "İ"      // LATIN CAPITAL LETTER I WITH DOT ABOVE: lowercases to two runes
	dotAbove = "\u0307" // COMBINING DOT ABOVE: the second of those two
	capSharp = "ẞ"      // LATIN CAPITAL LETTER SHARP S
	sharpS   = "ß"      // LATIN SMALL LETTER SHARP S
	nbsp     = "\u00a0" // NO-BREAK SPACE: whitespace to Python's split(), so a separator
)

func TestParseSHA256LineFixture(t *testing.T) {
	// The one sha256sums case in the manifest is the negative one: a checksum file
	// that names some other asset. Nothing published a digest for us, and "" would
	// be indistinguishable from a corrupt download.
	got, ok := ParseSHA256Line(fixture.Text(t, "sha256sums/other_file_only.txt"), assetName)
	if ok || got != "" {
		t.Errorf("ParseSHA256Line(other_file_only) = %q, %v, want \"\", false", got, ok)
	}
}

// TestParseSHA256Line is the measured table: every expectation here is what
// CPython 3.14.6's parse_sha256_line returned for the same two strings, with a
// Python None written as ok=false.
func TestParseSHA256Line(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		asset string
		want  string
		ok    bool
	}{
		{"empty", "", assetName, "", false},
		{"match", "abc123  " + assetName + "\n", assetName, "abc123", true},
		{"uppercase digest", "ABC123  " + assetName + "\n", assetName, "abc123", true},
		{"single space", "abc123 " + assetName + "\n", assetName, "abc123", true},
		{"tab separator", "abc123\t" + assetName + "\n", assetName, "abc123", true},
		{"many spaces", "abc123      " + assetName + "\n", assetName, "abc123", true},

		// One "*" is what coreutils writes for binary mode; a run of them is
		// stripped too, because Python lstrips a character set rather than a prefix.
		{"binary star", "abc123 *" + assetName + "\n", assetName, "abc123", true},
		{"many stars", "abc123 ***" + assetName + "\n", assetName, "abc123", true},
		{"asset name is only stars", "abc  ***\n", "", "abc", true},
		{"star in the middle", "abc123 fleet*fix\n", "fleet*fix", "abc123", true},

		{"no match", "abc123  some-other-file\n", assetName, "", false},
		{"second line matches", "abc123  other\ndef456  " + assetName + "\n", assetName, "def456", true},
		{"first match wins", "aaa  " + assetName + "\nbbb  " + assetName + "\n", assetName, "aaa", true},

		{"comment line skipped", "# note\nabc  " + assetName + "\n", assetName, "abc", true},
		{"indented comment skipped", "   # abc  " + assetName + "\n", assetName, "", false},
		{"hash mid-line is not a comment", "abc#123  " + assetName + "\n", assetName, "abc#123", true},

		// Two fields exactly. One is a bare filename list; three means the name
		// field still has the third field attached to it, so it matches nothing.
		{"one field only", assetName + "\n", assetName, "", false},
		{"three fields", "abc  " + assetName + "  extra\n", assetName, "", false},
		{"name with a space", "abc  fleetfix linux\n", "fleetfix linux", "abc", true},
		{"empty asset name", "abc  \n", "", "", false},

		{"indented line", "   abc  " + assetName + "   \n", assetName, "abc", true},
		{"no trailing newline", "abc  " + assetName, assetName, "abc", true},

		// Python's split(None, …) separates on its whole whitespace set, and
		// splitlines() breaks on its whole line-break set -- both wider than ASCII.
		{"nbsp separator", "abc" + nbsp + assetName + "\n", assetName, "abc", true},
		{"nbsp inside the name", "abc " + assetName + nbsp + "x\n", assetName, "", false},
		{"file separator line break", "zzz  other\x1cabc  " + assetName, assetName, "abc", true},

		// The digest is lowercased before it is returned, and by pytext.Lower rather
		// than strings.ToLower, so these four agree with Python rather than with Go.
		{"dotless i digest", "abc" + dotlessI + "  " + assetName + "\n", assetName, "abc" + dotlessI, true},
		{"dotted I digest", "ABC" + dottedI + "  " + assetName + "\n", assetName, "abci" + dotAbove, true},
		{"kelvin digest", "AB" + kelvin + "  " + assetName + "\n", assetName, "abk", true},
		{"sharp s digest", "AB" + capSharp + "  " + assetName + "\n", assetName, "ab" + sharpS, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := ParseSHA256Line(tt.text, tt.asset)
			if got != tt.want || ok != tt.ok {
				t.Errorf("ParseSHA256Line(%q, %q) = %q, %v, want %q, %v",
					tt.text, tt.asset, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func FuzzParseSHA256Line(f *testing.F) {
	f.Add("abc123  "+assetName+"\n", assetName)
	f.Add("", "")
	f.Add("# c\n\n   \nABC  *"+assetName, assetName)
	f.Add("abc  a b c\n", "a b c")
	f.Add("AB"+kelvin+"  x\n", "x")
	f.Add("a\x1cb c", "c")

	f.Fuzz(func(t *testing.T, text, asset string) {
		got, ok := ParseSHA256Line(text, asset)
		if !ok {
			if got != "" {
				t.Fatalf("ParseSHA256Line(%q, %q) = %q with ok=false, want empty", text, asset, got)
			}
			return
		}
		// A digest is the first field of a line, so it cannot be empty and cannot
		// hold whitespace -- if either happens the split has been misread.
		if got == "" {
			t.Fatalf("ParseSHA256Line(%q, %q) reported a match with an empty digest", text, asset)
		}
		if strings.IndexFunc(got, pytext.IsSpace) >= 0 {
			t.Fatalf("ParseSHA256Line(%q, %q) = %q, which contains whitespace", text, asset, got)
		}
		// It is returned lowercased, and lowercasing is idempotent, so lowering it
		// again must not move it. A digest that changes here was never lowered.
		if again := pytext.Lower(got); again != got {
			t.Fatalf("ParseSHA256Line(%q, %q) = %q, not lowercased (Lower gives %q)",
				text, asset, got, again)
		}
	})
}
