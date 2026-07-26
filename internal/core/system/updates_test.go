package system

import (
	"errors"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// upgradableLine is the line the security pattern needs to have something to
// attach to, so most of the security cases below are it plus one more line.
const upgradableLine = "3 packages can be updated.\n"

func TestParseNotifierTextFixtures(t *testing.T) {
	tests := []struct {
		fixture              string
		upgradable, security int64
		ok                   bool
	}{
		{"update_notifier/no_security.txt", 3, 0, true},
		{"update_notifier/with_security.txt", 14, 8, true},
		{"update_notifier/unrecognised.txt", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			u, s, ok := ParseNotifierText(fixture.Text(t, tt.fixture))
			if u != tt.upgradable || s != tt.security || ok != tt.ok {
				t.Errorf("ParseNotifierText(%s) = %d, %d, %v, want %d, %d, %v",
					tt.fixture, u, s, ok, tt.upgradable, tt.security, tt.ok)
			}
		})
	}
}

// TestParseNotifierText is the measured table: every expectation here is what
// CPython 3.14.6 returned for the same string.
func TestParseNotifierText(t *testing.T) {
	tests := []struct {
		name                 string
		text                 string
		upgradable, security int64
		ok                   bool
	}{
		{"plain", "3 packages can be updated.", 3, 0, true},
		{"singular", "1 package can be updated.", 1, 0, true},
		{"zero", "0 packages can be updated.", 0, 0, true},
		{"empty", "", 0, 0, false},
		{"unrecognised", "Welcome to Ubuntu\n", 0, 0, false},
		{"leading text", "note: 3 packages can be updated", 3, 0, true},
		{
			"with security",
			"12 packages can be updated.\n5 of these updates are security updates.\n",
			12, 5, true,
		},
		{
			"security first",
			"5 of these updates are security updates.\n12 packages can be updated.\n",
			12, 5, true,
		},
		{
			"two matches takes the first",
			"3 packages can be updated\n7 packages can be updated", 3, 0, true,
		},

		// The count and the words around it.
		{"no space before packages", "3packages can be updated", 0, 0, false},
		{"tabs between words", "3\tpackages\tcan\tbe\tupdated", 3, 0, true},
		{"newline between words", "3\npackages\ncan\nbe\nupdated", 3, 0, true},
		// \s is Unicode in Python, so a non-breaking space separates words.
		{"nbsp between words", "3\u00a0packages\u00a0can\u00a0be\u00a0updated", 3, 0, true},
		// \d is Unicode too: Arabic-Indic three, which int() then accepts.
		{"unicode digits", "٣ packages can be updated", 3, 0, true},

		// Where the security pattern will and will not anchor.
		{"security indented", upgradableLine + "   5 security updates\n", 3, 5, true},
		{"security on the first line", "5 security updates\n" + upgradableLine, 3, 5, true},
		{"security needs line start", upgradableLine + "blah 5 of these are security updates\n", 3, 0, true},
		{"no digit before security", upgradableLine + "some security updates\n", 3, 0, true},
		{"security zero", upgradableLine + "0 security updates\n", 3, 0, true},
		{"security greedy takes the first line", upgradableLine + "5 security updates\n9 security updates\n", 3, 5, true},
		{"security carriage return line", "3 packages can be updated.\r\n5 security updates\r\n", 3, 5, true},
		// v1 does not require the count and the phrase to be on separate lines, so
		// the pattern anchors on the upgradable count and reports it twice. Wrong
		// about the host, faithful to v1; see ParseNotifierText.
		{"security same line as the count", "3 packages can be updated, 5 security updates", 3, 3, true},
		{"security across a newline", upgradableLine + "5\nsecurity updates\n", 3, 0, true},
		{"security word not update", upgradableLine + "5 security patches\n", 3, 0, true},
		{"security singular", upgradableLine + "5 security update\n", 3, 5, true},
		{"security unicode digit", upgradableLine + "٥ security updates\n", 3, 5, true},

		// The \b after the count: a non-word character passes, a word character
		// does not, and the boundary is not allowed to be the line ending.
		{"security digit then punctuation", upgradableLine + "5. security updates\n", 3, 5, true},
		{"security digit glued to a letter", upgradableLine + "5a security updates\n", 3, 0, true},
		{"security digit then a non-ASCII letter", upgradableLine + "5é security updates\n", 3, 0, true},
		// U+0345 COMBINING GREEK YPOGEGRAMMENI is not a word character to Python,
		// so it is a boundary. It is also the one rune Go's (?i) would fold into a
		// negated word class, which is why the class carries (?-i:...).
		{"security digit then a combining mark", upgradableLine + "5\u0345 security updates\n", 3, 5, true},

		// Case folding, where the two engines had to be measured rather than
		// assumed. All four of these match in CPython.
		{"security uppercase", upgradableLine + "5 SECURITY UPDATES\n", 3, 5, true},
		{"security long s", upgradableLine + "5 ſecurity updates\n", 3, 5, true},
		{"security dotless i", upgradableLine + "5 securıty updates\n", 3, 5, true},
		{"security dotted capital i", upgradableLine + "5 securİty updates\n", 3, 5, true},
		{"packages uppercase", "3 PACKAGES CAN BE UPDATED", 3, 0, true},
		{"packages mixed case", "3 Packages Can Be Updated", 3, 0, true},
		{"packages long s", "3 packageſ can be updated", 3, 0, true},
		{"packages kelvin sign", "3 pac\u212aages can be updated", 3, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, s, ok := ParseNotifierText(tt.text)
			if u != tt.upgradable || s != tt.security || ok != tt.ok {
				t.Errorf("ParseNotifierText(%q) = %d, %d, %v, want %d, %d, %v",
					tt.text, u, s, ok, tt.upgradable, tt.security, tt.ok)
			}
		})
	}
}

// TestParseNotifierTextHugeCountHasNoAnswer covers the one place this parser
// departs from v1. Python's ints are arbitrary precision and returns 10^30-1
// for both of these; int64 cannot, so the reading is refused rather than
// truncated. Reachable only from a notifier file nothing writes.
func TestParseNotifierTextHugeCountHasNoAnswer(t *testing.T) {
	huge := strings.Repeat("9", 30)
	tests := []struct {
		name string
		text string
	}{
		{"upgradable", huge + " packages can be updated"},
		{"security", upgradableLine + huge + " security updates\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, s, ok := ParseNotifierText(tt.text)
			if ok {
				t.Errorf("ParseNotifierText(%q) = %d, %d, true, want no answer", tt.text, u, s)
			}
		})
	}
}

// TestNotifierCountsOnlyFailAsOutOfRange pins the assumption the departure above
// rests on: both patterns capture a run of \d, every one of which int() accepts,
// so a capture can only fail conversion by being too large. If Int ever rejected
// one of these for syntax the parser would silently report no answer for a count
// it should have read.
func TestNotifierCountsOnlyFailAsOutOfRange(t *testing.T) {
	for _, digits := range []string{"0", "9", "42", "٣", "٥", "०१", strings.Repeat("9", 30)} {
		t.Run(digits, func(t *testing.T) {
			if !pytext.IsDigitString(digits) {
				t.Fatalf("%q is not a digit run; the case is not testing what it claims", digits)
			}
			_, err := pytext.Int(digits)
			if err != nil && !errors.Is(err, pytext.ErrRange) {
				t.Errorf("pytext.Int(%q) failed with %v, want nil or ErrRange", digits, err)
			}
		})
	}
}

func TestParseAptUpgradableFixtures(t *testing.T) {
	tests := []struct {
		fixture              string
		upgradable, security int64
	}{
		{"apt/upgradable_one.txt", 1, 0},
		{"apt/upgradable_three.txt", 3, 1},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			u, s := ParseAptUpgradable(fixture.Text(t, tt.fixture))
			if u != tt.upgradable || s != tt.security {
				t.Errorf("ParseAptUpgradable(%s) = %d, %d, want %d, %d",
					tt.fixture, u, s, tt.upgradable, tt.security)
			}
		})
	}
}

func TestParseAptUpgradable(t *testing.T) {
	tests := []struct {
		name                 string
		text                 string
		upgradable, security int64
	}{
		{"empty", "", 0, 0},
		{"listing header only", "Listing...\n", 0, 0},
		{
			"one package",
			"Listing...\nvim/jammy-updates 2:8.2.3995-1ubuntu2.9 amd64 [upgradable from: 2:8.2.3995-1]\n",
			1, 0,
		},
		{
			"one security",
			"Listing...\nopenssl/jammy-security 3.0.2-0ubuntu1.10 amd64 [upgradable from: 3.0.2-0ubuntu1.9]\n",
			1, 1,
		},
		{
			"mixed",
			"Listing...\nvim/jammy-updates 2:8.2 amd64 [upgradable from: 2:8.1]\n" +
				"openssl/jammy-security 3.0.2 amd64 [upgradable from: 3.0.1]\n",
			2, 1,
		},

		// Which lines count as a row.
		{"blank lines skipped", "\n\nvim/jammy 1.0\n\n", 1, 0},
		{"whitespace only line skipped", "   \n\tvim/jammy 1.0\n", 1, 0},
		{"no trailing newline", "vim/jammy 1.0", 1, 0},
		// str.splitlines, so a form feed ends a line the way a newline does.
		{"exotic line boundary", "vim/jammy 1.0\u000copenssl/jammy-security 1.0", 2, 1},
		{"listing prefix matches anywhere at the start", "Listing packages...\nvim/jammy 1.0\n", 1, 0},
		// The header test runs after the strip, so an indented one is still a
		// header -- and it is case-sensitive, so a lowercased one is a package.
		{"indented listing is still skipped", "   Listing...\nvim/jammy 1.0\n", 1, 0},
		{"listing lowercase is counted", "listing...\nvim/jammy 1.0\n", 2, 0},

		// Which rows count as security. The suite is everything up to the first
		// literal space, so any other separator leaves the whole line as the suite.
		{"leading whitespace on the row", "   vim/jammy-security 1.0\n", 1, 1},
		{"two spaces between fields", "vim/jammy-security  1.0\n", 1, 1},
		{"no space at all", "vim/jammy-security\n", 1, 1},
		{"tab separated", "vim/jammy-security\t1.0\tamd64\n", 1, 0},
		{"nbsp separated", "vim/jammy-security\u00a01.0\n", 1, 0},
		{"just a dash security", "-security 1.0\n", 1, 1},
		// The second test: a -security pocket named anywhere in the line.
		{"security substring elsewhere in the line", "pkg/jammy 1.0 amd64 [from jammy-security/main]\n", 1, 1},
		{"security in the middle", "vim/jammy-security/main 1.0\n", 1, 1},
		{"suite ends with -security but no slash", "jammy-security 1.0 amd64\n", 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, s := ParseAptUpgradable(tt.text)
			if u != tt.upgradable || s != tt.security {
				t.Errorf("ParseAptUpgradable(%q) = %d, %d, want %d, %d",
					tt.text, u, s, tt.upgradable, tt.security)
			}
		})
	}
}

func FuzzParseNotifierText(f *testing.F) {
	for _, seed := range []string{
		"", "3 packages can be updated.", upgradableLine + "5 security updates\n",
		"3\u00a0packages\u00a0can\u00a0be\u00a0updated", "٣ packages can be updated",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		u, s, ok := ParseNotifierText(text)
		if !ok {
			if u != 0 || s != 0 {
				t.Errorf("ParseNotifierText(%q) = %d, %d with ok false, want zeroes", text, u, s)
			}
			return
		}
		if u < 0 || s < 0 {
			t.Errorf("ParseNotifierText(%q) = %d, %d, want counts at or above zero", text, u, s)
		}
	})
}

func FuzzParseAptUpgradable(f *testing.F) {
	for _, seed := range []string{
		"", "Listing...\n", "vim/jammy-security 1.0\n",
		"pkg/jammy 1.0 amd64 [from jammy-security/main]\n",
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, text string) {
		u, s := ParseAptUpgradable(text)
		if u < 0 || s < 0 {
			t.Errorf("ParseAptUpgradable(%q) = %d, %d, want counts at or above zero", text, u, s)
		}
		if s > u {
			t.Errorf("ParseAptUpgradable(%q) = %d, %d: more security rows than rows", text, u, s)
		}
		if lines := int64(len(pytext.SplitLines(text))); u > lines {
			t.Errorf("ParseAptUpgradable(%q) counted %d rows in %d lines", text, u, lines)
		}
	})
}
