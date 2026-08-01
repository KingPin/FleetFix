package system

import (
	"regexp"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// The two shapes v1 reads an available-update count out of.
//
// Both are IGNORECASE in Python and (?i) here, and that flag is the whole reason
// these two patterns needed measuring rather than transcribing. Every letter in
// them was checked rune by rune against CPython's re: for all of p a c k g e s n
// b u d t r y the two engines fold identically, including the two folds that
// look like bugs and are not -- (?i)k matches U+212A KELVIN SIGN and (?i)s
// matches U+017F LATIN SMALL LETTER LONG S in both. The one letter they disagree
// on is i: Python additionally matches U+0130 and U+0131, the dotted and dotless
// Turkish forms, which Go's simple folding does not. "security" is the only word
// here containing an i, so its i is spelled as an explicit class rather than
// leaving a divergence for a Turkish-locale notifier string to find.
var (
	// notifierRegular is v1's _NOTIFIER_REGULAR:
	//   r"(\d+)\s+package(?:s)?\s+can\s+be\s+updated"
	notifierRegular = regexp.MustCompile(
		`(?i)(` + pytext.Digit + `+)` + pytext.Space + `+package(?:s)?` +
			pytext.Space + `+can` + pytext.Space + `+be` + pytext.Space + `+updated`,
	)

	// notifierSecurity is v1's _NOTIFIER_SECURITY:
	//   r"^\s*(\d+)\b.*security\s+update"   (MULTILINE)
	notifierSecurity = regexp.MustCompile(
		`(?im)^` + pytext.Space + `*(` + pytext.Digit + `+)` + afterWord +
			`.*secur` + latinI + `ty` + pytext.Space + `+update`,
	)
)

// afterWord stands in for the \b that follows the captured count.
//
// Go has no \b honouring Unicode word characters, so the boundary is consumed
// rather than asserted. That is sound only in this position: the character
// before it is always a digit, so \b reduces to "the next character is not a
// word character", and whatever it is gets absorbed by the .* that follows.
//
// Two details carry weight. Newline is excluded because Python's \b is
// zero-width and its . does not cross a line -- "5\nsecurity updates" is not a
// match in v1, and a boundary class that accepted the newline would make it one.
// And the class is wrapped in (?-i:...) because Go folds a negated class before
// negating it: under (?i) the class stops matching U+0345 COMBINING GREEK
// YPOGEGRAMMENI, which folds to a letter, while Python's \w never counted it as
// a word character. That one rune is the entire difference, and turning the flag
// off for the class removes it.
const afterWord = `(?-i:[^\p{L}\p{N}_\n])`

// latinI matches what Python's (?i)i matches: I, i, and the two Turkish forms
// U+0130 LATIN CAPITAL LETTER I WITH DOT ABOVE and U+0131 LATIN SMALL LETTER
// DOTLESS I. Listing the pair is enough -- each folds only to itself in Go, so
// the surrounding (?i) adds the ASCII capital and nothing else.
const latinI = `[i\x{0130}\x{0131}]`

// ParseNotifierText reads the counts out of /var/lib/update-notifier/updates-available,
// the pre-rendered MOTD fragment Ubuntu writes. ok is false when the file holds
// no recognisable count, which is the common case on a host with nothing to
// install: the notifier writes an empty file rather than a zero.
//
// The security count is deliberately not required to be on its own line, because
// v1 does not require it either, and the consequence is worth stating: for
// "3 packages can be updated, 5 security updates" the security pattern anchors
// on the 3 at the start of the line, not on the 5, and both counts come back as
// 3. Faithful to v1, wrong about the host, and reproduced rather than fixed so
// that the port is not the thing that changes an operator's numbers. Fixing it
// belongs in a v2 behaviour change with its own fixture.
//
// One departure: v1's ints are arbitrary precision and these are int64, so a
// count past 2^63-1 is reported as no answer instead of as itself. A notifier
// claiming more upgradable packages than the archive has ever contained is not a
// reading worth passing on, and silently truncating it to a smaller number --
// or worse, reporting zero security updates -- would be.
func ParseNotifierText(text string) (upgradable, security int64, ok bool) {
	m := notifierRegular.FindStringSubmatch(text)
	if m == nil {
		return 0, 0, false
	}
	upgradable, err := pytext.Int(m[1])
	if err != nil {
		return 0, 0, false
	}

	sm := notifierSecurity.FindStringSubmatch(text)
	if sm == nil {
		return upgradable, 0, true
	}
	security, err = pytext.Int(sm[1])
	if err != nil {
		return 0, 0, false
	}
	return upgradable, security, true
}

// ParseAptUpgradable counts the rows of `apt list --upgradable`. Every line that
// is neither blank nor the "Listing..." header is one upgradable package; a
// package is security if its suite ends in -security, or if the line mentions a
// -security pocket anywhere, which is how the "[upgradable from: ...]" tail
// reports a package pulled from the security archive.
//
// Both of those tests are as literal as v1's, and that matters more than it
// looks. The header check is case-sensitive, so a "listing..." line is counted
// as a package. The suite is taken with a split on one literal space rather than
// on whitespace, so a tab-separated row has its whole line as the suite and does
// not end in -security. Neither is right, both are v1, and apt's real output
// hits neither.
func ParseAptUpgradable(text string) (upgradable, security int64) {
	for _, raw := range pytext.SplitLines(text) {
		line := strings.TrimFunc(raw, pytext.IsSpace)
		if line == "" || strings.HasPrefix(line, "Listing") {
			continue
		}
		upgradable++

		suite, _, _ := strings.Cut(line, " ")
		if strings.HasSuffix(suite, "-security") || strings.Contains(line, "-security/") {
			security++
		}
	}
	return upgradable, security
}
