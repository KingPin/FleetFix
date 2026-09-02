package updater

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// IsNewer reports whether the remote version is strictly greater than the local one.
//
// PEP 440, not semver, because that is what v1 compares with: checker.py's docstring
// says "parses as semver" and its code calls packaging.version.Version, and the code
// is what shipped. The two orderings agree on every tag this project has published
// and disagree readily off that path -- 2.1 is a version to PEP 440 and not to
// semver, 1.0.0.post1 likewise, and 1.0.0-alpha.1 is a version to semver and not to
// PEP 440. Porting the docstring instead of the code would have moved the boundary
// between "offer this update" and "stay quiet" without anyone choosing to.
//
// An unparseable version on either side is not newer, which is v1's InvalidVersion
// branch and is the safe direction: a tag nobody can order is not grounds for
// replacing the binary an operator is running.
func IsNewer(remote, local string) bool {
	r, ok := parsePEP440(stripV(remote))
	if !ok {
		return false
	}
	l, ok := parsePEP440(stripV(local))
	if !ok {
		return false
	}
	return compare440(r, l) > 0
}

// Sort ranks, straight from packaging's _cmpkey. A dev release with no pre-release
// of its own sorts before every alpha; a release with no pre-release at all sorts
// after every rc.
const (
	preRankDevOnly = -1
	preRankAlpha   = 0
	preRankBeta    = 1
	preRankRC      = 2
	preRankStable  = 3

	// In a local version segment, strings sort before numbers.
	localStringRank = -1
)

// version440 is a PEP 440 version reduced to the tuple packaging compares.
//
// The parsed spelling is not kept. Nothing here renders a version back, and holding
// both would invite a caller to compare the spellings instead -- which is the bug
// this type exists to prevent, since 1.0, 1.0.0 and 1.0.0.0 are one version.
type version440 struct {
	epoch int

	// release with trailing zeros stripped, so 1.0.0 and 1 compare equal.
	release []int

	// suffix is (preRank, preN, postRank, postN, devRank, devN), flattened the way
	// packaging flattens it so one tuple comparison orders all three segments.
	suffix [6]int

	// local is absent for most versions, and its absence sorts before any present
	// one: 1.0 < 1.0+ubuntu1. A nil slice would say the same thing, but a version
	// with an empty local segment cannot be spelled, so the flag costs nothing and
	// keeps the comparison from having to know that.
	local    []localSegment
	hasLocal bool
}

// localSegment is one dot-separated piece of a local version, encoded so that
// numbers and words are comparable to each other: a number carries its value and an
// empty string, a word carries localStringRank and its text.
type localSegment struct {
	rank int
	text string
}

// pep440Pattern is packaging's VERSION_PATTERN, unwrapped from re.VERBOSE.
//
// Case folding is not (?i) here. Python applies it inside (?a:...), so the folding is
// ASCII-only, where Go's (?i) folds by Unicode simple case rules -- under which
// U+017F LATIN SMALL LETTER LONG S matches the "s" in "post" and U+212A KELVIN SIGN
// matches a "k" in a local segment. parsePEP440 lowercases the ASCII letters itself
// instead, which is exactly what the Python does and nothing more.
//
// Possessive quantifiers are dropped with them. Go's regexp has no backtracking, and
// packaging ships the same pattern without them for Python below 3.11.5.
var pep440Pattern = regexp.MustCompile(
	`\Av?` +
		`(?:(?:(?P<epoch>[0-9]+)!)?` +
		`(?P<release>[0-9]+(?:\.[0-9]+)*)` +
		`(?:[._-]?(?P<pre_l>alpha|a|beta|b|preview|pre|c|rc)[._-]?(?P<pre_n>[0-9]+)?)?` +
		`(?:(?:-(?P<post_n1>[0-9]+))|(?:[._-]?(?P<post_l>post|rev|r)[._-]?(?P<post_n2>[0-9]+)?))?` +
		`(?:[._-]?(?P<dev_l>dev)[._-]?(?P<dev_n>[0-9]+)?)?)` +
		`(?:\+(?P<local>[a-z0-9]+(?:[._-][a-z0-9]+)*))?` +
		`\z`,
)

// pep440Group indexes the named submatches once, so the parser is not looking names
// up by string on every call.
var pep440Group = func() map[string]int {
	idx := map[string]int{}
	for i, name := range pep440Pattern.SubexpNames() {
		if name != "" {
			idx[name] = i
		}
	}
	return idx
}()

// preNormalization is packaging's _LETTER_NORMALIZATION for the pre-release segment:
// several spellings of three things.
var preNormalization = map[string]string{
	"alpha":   "a",
	"beta":    "b",
	"c":       "rc",
	"pre":     "rc",
	"preview": "rc",
}

// preRank orders the normalized pre-release letters.
var preRank = map[string]int{"a": preRankAlpha, "b": preRankBeta, "rc": preRankRC}

// simpleIndicators is packaging's fast path: a version made only of these characters
// skips the regex entirely and is split on dots.
const simpleIndicators = ".0123456789"

// parsePEP440 reports the version's comparison form, or false for InvalidVersion.
func parsePEP440(v string) (version440, bool) {
	// The fast path runs on the untrimmed string, as packaging's does, which is why
	// " 1.0" reaches the regex and "1.0" does not. Both answer the same; the order
	// only matters for keeping the two paths' edge cases aligned. Note that the empty
	// string is a subset of every set, so it lands here and fails on the empty part.
	if strings.IndexFunc(v, func(r rune) bool { return !strings.ContainsRune(simpleIndicators, r) }) < 0 {
		release, ok := releaseParts(v)
		if !ok {
			return version440{}, false
		}
		return version440{
			release: stripTrailingZeros(release),
			suffix:  [6]int{preRankStable, 0, 0, 0, 1, 0},
		}, true
	}

	// \s* on both ends of packaging's compiled pattern, where \s on a str pattern is
	// str.isspace() per character -- so a non-breaking space is stripped too, which
	// Go's own \s would not have matched.
	m := pep440Pattern.FindStringSubmatch(asciiLower(strings.TrimFunc(v, pytext.IsSpace)))
	if m == nil {
		return version440{}, false
	}
	group := func(name string) string { return m[pep440Group[name]] }

	epoch := 0
	if raw := group("epoch"); raw != "" {
		var ok bool
		if epoch, ok = atoi440(raw); !ok {
			return version440{}, false
		}
	}
	release, ok := releaseParts(group("release"))
	if !ok {
		return version440{}, false
	}

	// _parse_letter_version, three times. An implicit 0 where a segment carries a
	// letter and no number, so 1.0rc and 1.0rc0 are one version.
	preLetter, preN, hasPre, ok := letterVersion(group("pre_l"), group("pre_n"))
	if !ok {
		return version440{}, false
	}
	// The letter-less post form, `1.0-1`. packaging feeds post_n1 in as the number
	// with no letter, and the parser reads that as an implicit "post".
	postNumber := group("post_n1")
	if postNumber == "" {
		postNumber = group("post_n2")
	}
	_, postN, hasPost, ok := letterVersion(group("post_l"), postNumber)
	if !ok {
		return version440{}, false
	}
	hasPost = hasPost || postNumber != ""
	_, devN, hasDev, ok := letterVersion(group("dev_l"), group("dev_n"))
	if !ok {
		return version440{}, false
	}

	parsed := version440{epoch: epoch, release: stripTrailingZeros(release)}
	switch {
	case !hasPre && !hasPost && hasDev:
		parsed.suffix[0] = preRankDevOnly
	case !hasPre:
		parsed.suffix[0] = preRankStable
	default:
		parsed.suffix[0], parsed.suffix[1] = preRank[preNormalize(preLetter)], preN
	}
	if hasPost {
		parsed.suffix[2], parsed.suffix[3] = 1, postN
	}
	if !hasDev {
		parsed.suffix[4] = 1
	} else {
		parsed.suffix[5] = devN
	}

	if local := group("local"); local != "" {
		parsed.local, parsed.hasLocal = localSegments(local), true
	}
	return parsed, true
}

// letterVersion is packaging's _parse_letter_version: a letter with an implicit zero,
// or a bare number read as an implicit post release, or nothing.
func letterVersion(letter, number string) (string, int, bool, bool) {
	if letter == "" && number == "" {
		return "", 0, false, true
	}
	if number == "" {
		return letter, 0, true, true
	}
	n, ok := atoi440(number)
	if !ok {
		return "", 0, false, false
	}
	return letter, n, true, true
}

func preNormalize(letter string) string {
	if normal, ok := preNormalization[letter]; ok {
		return normal
	}
	return letter
}

// releaseParts splits and parses the release segment. An empty part is what makes
// "1..2" and "" invalid rather than a release of zero.
func releaseParts(s string) ([]int, bool) {
	parts := strings.Split(s, ".")
	out := make([]int, len(parts))
	for i, part := range parts {
		n, ok := atoi440(part)
		if !ok {
			return nil, false
		}
		out[i] = n
	}
	return out, true
}

// stripTrailingZeros is packaging's release trim, so 1.0.0 compares equal to 1.
func stripTrailingZeros(release []int) []int {
	i := len(release)
	for i > 0 && release[i-1] == 0 {
		i--
	}
	return release[:i]
}

// localSegments splits a local version on any of PEP 440's three separators and
// encodes each piece so numbers and words can be compared to each other.
func localSegments(local string) []localSegment {
	parts := strings.FieldsFunc(local, func(r rune) bool {
		return r == '.' || r == '_' || r == '-'
	})
	out := make([]localSegment, 0, len(parts))
	for _, part := range parts {
		// str.isdigit() and then int(), so a segment past what an int can hold is a
		// word here rather than a number. See atoi440 on why that is the direction to
		// fail in.
		if n, ok := atoi440(part); ok && isASCIIDigits(part) {
			out = append(out, localSegment{rank: n})
			continue
		}
		out = append(out, localSegment{rank: localStringRank, text: part})
	}
	return out
}

// compare440 orders two versions the way packaging's tuple comparison does.
func compare440(a, b version440) int {
	if c := cmpInt(a.epoch, b.epoch); c != 0 {
		return c
	}
	if c := cmpIntSlice(a.release, b.release); c != 0 {
		return c
	}
	for i := range a.suffix {
		if c := cmpInt(a.suffix[i], b.suffix[i]); c != 0 {
			return c
		}
	}
	// A three-element key against a four-element one: no local sorts before any
	// local, which is Python comparing tuples of different lengths after a common
	// prefix.
	if a.hasLocal != b.hasLocal {
		if a.hasLocal {
			return 1
		}
		return -1
	}
	for i := 0; i < len(a.local) && i < len(b.local); i++ {
		if c := cmpInt(a.local[i].rank, b.local[i].rank); c != 0 {
			return c
		}
		if c := strings.Compare(a.local[i].text, b.local[i].text); c != 0 {
			return c
		}
	}
	return cmpInt(len(a.local), len(b.local))
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// cmpIntSlice compares element by element and then by length, which is how Python
// orders two tuples where one is a prefix of the other.
func cmpIntSlice(a, b []int) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if c := cmpInt(a[i], b[i]); c != 0 {
			return c
		}
	}
	return cmpInt(len(a), len(b))
}

// atoi440 parses one numeric component.
//
// Python's int is unbounded, so a release component of twenty digits is a version
// there and is out of range here. It is refused rather than saturated: saturating
// would make two distinct versions compare equal, and the caller's answer to a
// version it cannot order is that there is no update -- which is the direction that
// does not replace a running binary on the strength of a number nobody can read.
func atoi440(s string) (int, bool) {
	if s == "" || !isASCIIDigits(s) {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

func isASCIIDigits(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return len(s) > 0
}

// asciiLower folds A-Z and leaves every other byte alone, which is what matching
// under Python's (?a:...) with re.IGNORECASE does. strings.ToLower would fold by
// Unicode rules and let a Kelvin sign into a local version segment.
func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}
