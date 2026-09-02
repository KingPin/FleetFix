package updater

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// TestIsNewerMatchesPackaging replays the pin written by tools/oracle/pin_pep440.py.
//
// Every ordered pair of the corpus, against the answer the real packaging.version
// gave for it. A hand-written table would have been wrong in exactly the places this
// port is: the ranks that put a dev release before an alpha, the implicit zero in
// 1.0rc, the local segment where a word sorts under a number.
func TestIsNewerMatchesPackaging(t *testing.T) {
	var corpus []string
	readPin(t, "pep440/corpus.json", &corpus)

	var pin struct {
		IsNewer []string `json:"is_newer"`
	}
	readPin(t, "pep440/pin.json", &pin)

	if len(pin.IsNewer) != len(corpus) {
		t.Fatalf("pin has %d rows for %d cases; regenerate it", len(pin.IsNewer), len(corpus))
	}
	for i, remote := range corpus {
		row := pin.IsNewer[i]
		if len(row) != len(corpus) {
			t.Fatalf("row %d has %d columns for %d cases; regenerate it", i, len(row), len(corpus))
		}
		for j, local := range corpus {
			want := row[j] == '1'
			if got := IsNewer(remote, local); got != want {
				t.Errorf("IsNewer(%q, %q) = %v, want %v", remote, local, got, want)
			}
		}
	}
}

func readPin(t *testing.T, name string, into any) {
	t.Helper()
	data, err := os.ReadFile(fixture.Path(name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	if err := json.Unmarshal(data, into); err != nil {
		t.Fatalf("decoding %s: %v", name, err)
	}
}

// The tag GitHub sends carries a "v" and the local version does not, which is the
// only pairing that happens in production.
func TestIsNewerComparesATagAgainstABareVersion(t *testing.T) {
	if !IsNewer("v2.1.0", "2.0.0") {
		t.Error("v2.1.0 is newer than 2.0.0")
	}
	if IsNewer("v2.0.0", "2.0.0") {
		t.Error("a release equal to the running one is not newer")
	}
	if IsNewer("v1.9.0", "2.0.0") {
		t.Error("an older release is not newer")
	}
}

// The version package's fallback, which reaches the checker on a build that carries
// no stamp. It must not parse as something a release can beat, and it must not make
// every release look like an update either.
func TestADevBuildIsNeverOfferedAnUpdate(t *testing.T) {
	if IsNewer("v2.0.0", "dev") {
		t.Error(`"dev" does not parse, so nothing is newer than it`)
	}
}

// A release-candidate tag must not overtake the release it precedes -- an operator
// on 2.0.0 being offered 2.0.0rc1 is the visible form of getting these ranks wrong.
func TestAPreReleaseDoesNotOvertakeItsRelease(t *testing.T) {
	if IsNewer("v2.0.0rc1", "2.0.0") {
		t.Error("2.0.0rc1 precedes 2.0.0")
	}
	if !IsNewer("v2.0.0", "2.0.0rc1") {
		t.Error("2.0.0 follows 2.0.0rc1")
	}
}

// A release component past a 64-bit integer. Python's int is unbounded and orders it;
// this refuses it, which is the direction that does not replace a running binary on
// the strength of a number nobody can read. Asserted rather than left implicit,
// because it is the port's one deliberate difference from packaging.
func TestAnOversizedComponentIsRefusedRatherThanSaturated(t *testing.T) {
	huge := "99999999999999999999.0"
	if IsNewer(huge, "1.0") {
		t.Error("an unorderable version is not newer")
	}
	if IsNewer("1.0", huge) {
		t.Error("nothing is newer than an unorderable version either")
	}
}

func TestParseRejectsAnOversizedEpochAndLocalSegment(t *testing.T) {
	// Every numeric component the parser has to turn into an int, one per segment.
	// The release one carries a "v" so it takes the pattern rather than the
	// all-digits fast path the test above exercises.
	for _, v := range []string{
		"v99999999999999999999.0",
		"99999999999999999999!1.0",
		"1.0a99999999999999999999",
		"1.0.post99999999999999999999",
		"1.0.dev99999999999999999999",
	} {
		if _, ok := parsePEP440(v); ok {
			t.Errorf("parsePEP440(%q) succeeded; the component does not fit an int", v)
		}
	}
	// A local segment is a word when it will not fit, rather than a failed parse:
	// packaging tests isdigit() and then int(), and only the release, epoch and
	// letter numbers are required to be numbers.
	parsed, ok := parsePEP440("1.0+99999999999999999999")
	if !ok {
		t.Fatal("a local segment that will not fit an int is still a version")
	}
	if len(parsed.local) != 1 || parsed.local[0].rank != localStringRank {
		t.Errorf("got %#v, want the segment kept as a word", parsed.local)
	}
}

// asciiLower must not fold by Unicode rules: U+017F folds onto "s" under Unicode
// simple folding, which would let "po<U+017F>t" match the "post" literal. Python
// matches under (?a:...), so it does not.
func TestFoldingIsASCIIOnly(t *testing.T) {
	if _, ok := parsePEP440("1.0.poſt1"); ok {
		t.Error("a long s is not an s to an ASCII-mode pattern")
	}
	if got := asciiLower("1.0-RC1"); got != "1.0-rc1" {
		t.Errorf("got %q, want 1.0-rc1", got)
	}
	if got := asciiLower("ſ"); got != "ſ" {
		t.Errorf("got %q, want the long s unchanged", got)
	}
}
