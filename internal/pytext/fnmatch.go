package pytext

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Python's fnmatch.fnmatchcase, the case-sensitive door -- the only one v1 uses
// (modules/storage/stale.py:139 classifies a filename against two glob lists).
// fnmatch() itself is fnmatchcase() with os.path.normcase applied to both
// arguments, which is the identity on POSIX; leaving that door out keeps a
// Windows-only case fold from being half-implemented here.
//
// Deliberately not path.Match. The two agree on * and ? over a name with no
// separator in it and disagree on nearly everything else:
//
//   - path.Match treats '\' as an escape. fnmatch does not -- "there is no way to
//     quote meta-characters", says its own docstring -- so "[a\b]" is a class of
//     three characters, one of which is a backslash.
//   - path.Match stops '*' at a '/'; fnmatch crosses it. v1 classifies a bare
//     filename, so this one is latent rather than live, but it is the kind of
//     latent that wakes up the first time a caller passes a path.
//   - path.Match returns ErrBadPattern for an unterminated '['; fnmatch treats it
//     as a literal '[' and carries on, so "[abc" matches the four-character name
//     "[abc".
//   - path.Match has no analogue of the empty-range and set-difference fixups
//     below: "[b-a]" is a syntax error there and a never-matching class here,
//     and "[a--b]" is a three-character class there and a one-character class
//     here.
//
// On the fourteen globs v1 actually ships the two happen to agree. A glob list is
// exactly the kind of thing an operator edits, so agreeing on today's list is not
// the property worth shipping.
//
// The implementation is a port of CPython's fnmatch.translate rather than an
// independent matcher, for the same reason internal/pytime is a port of
// fromisoformat: the fixups translate() applies to a hostile character class are
// not derivable from the documentation, which says only "[seq] matches any
// character in seq".
//
// Measured, not read off the grammar: every pattern up to five characters over
// the alphabet `*?[]!^-\ab` -- 111,110 of them -- was matched against all 400
// names up to three characters over `ab-]^!\`, on CPython 3.10 (what the
// differential oracle runs) and on 3.14 (what this machine runs). The two
// interpreters agree on every one, so unlike fromisoformat there is no
// version-skew boundary to arbitrate here.

// fnSetOps are the three characters `re` reserves for the set operations it does
// not implement yet (&&, ~~, ||). translate escapes every occurrence, not just
// the doubled ones, so a class stays a class if a future Python starts reading
// them.
var fnSetOps = regexp.MustCompile(`([&~|])`)

// FNPattern is a compiled fnmatch pattern. fnmatch caches its compiled regexes
// behind an lru_cache; this makes the cache the caller's business instead, so a
// walk over a home directory compiles each glob once rather than once per file.
//
// The zero value never matches, which is also what an empty character class
// translates to.
type FNPattern struct {
	re *regexp.Regexp
}

// CompileFNPattern translates a shell pattern into a matcher.
//
// There is no error return because fnmatch.translate has no failure mode: every
// str pattern translates, including the ones that translate to something
// unmatchable. FuzzFNTranslateAlwaysCompiles pins that.
func CompileFNPattern(pattern string) FNPattern {
	return newFNPattern(fnTranslate(pattern))
}

// newFNPattern is split out so the unreachable compile failure below is reachable
// from a test.
func newFNPattern(regex string, never bool) FNPattern {
	if never {
		return FNPattern{}
	}
	re, err := regexp.Compile(regex)
	if err != nil {
		// Never-match rather than a panic: a glob reaches here from an operator's
		// YAML, and a config typo must not take the process down. Unreachable in
		// practice, which is the fuzz target's job to keep true.
		return FNPattern{}
	}
	return FNPattern{re: re}
}

// MatchCase reports whether name matches, case included.
func (p FNPattern) MatchCase(name string) bool {
	return p.re != nil && p.re.MatchString(name)
}

// FNMatchCase is fnmatch.fnmatchcase(name, pat).
//
// The argument order is Python's -- name first, pattern second -- which is the
// opposite of path.Match's. Kept deliberately: a transposed call is a silent
// wrong answer rather than a compile error, so the two functions are easier to
// tell apart when they read differently.
//
// A caller matching many names against the same pattern wants CompileFNPattern;
// this one translates and compiles per call.
func FNMatchCase(name, pattern string) bool {
	return CompileFNPattern(pattern).MatchCase(name)
}

// fnTranslate is fnmatch.translate, emitting Go's regexp dialect.
//
// never reports a pattern that cannot match anything. CPython spells that as a
// `(?!)` in the concatenation, which RE2 has no lookahead to express; a flag says
// the same thing and short-circuits the compile.
//
// Two other spellings differ from CPython's output while describing the same
// language: `\A`/`\z` for its `re.match` + `\Z` anchoring, and a plain `.*` per
// star where 3.11+ emits the atomic group `(?>.*?fixed)`. The atomic group exists
// to stop a backtracking engine exploring every split of the input; Go's engine
// does not backtrack, and committing to the leftmost match of `fixed` cannot lose
// a match anyway, because whatever follows an interior star begins with another
// `.*` that absorbs the difference.
func fnTranslate(pattern string) (regex string, never bool) {
	pat := []rune(pattern)
	n := len(pat)

	var b strings.Builder
	b.WriteString(`\A(?s:`)
	for i := 0; i < n; {
		c := pat[i]
		i++
		switch c {
		case '*':
			b.WriteString(".*")
			// Consecutive stars collapse. `.*.*` describes the same language, so
			// this is only about handing RE2 a smaller program.
			for i < n && pat[i] == '*' {
				i++
			}
		case '?':
			b.WriteString(".")
		case '[':
			// The closing bracket is looked for from the first character that could
			// not be one: a ']' immediately after the '[' or after its '!' is a
			// member of the class, not its terminator.
			j := i
			if j < n && pat[j] == '!' {
				j++
			}
			if j < n && pat[j] == ']' {
				j++
			}
			for j < n && pat[j] != ']' {
				j++
			}
			if j >= n {
				// Unterminated: a literal '[', and the scan resumes at the very next
				// character rather than skipping what looked like a class.
				b.WriteString(`\[`)
				break
			}
			stuff := fnClassBody(pat, i, j)
			i = j + 1
			switch stuff {
			case "":
				// Every range in the class was empty. Not a class that matches
				// nothing -- a pattern that matches nothing, since it still has to
				// match one character here.
				never = true
			case "!":
				// A negated empty range: everything is outside it.
				b.WriteString(".")
			default:
				stuff = fnSetOps.ReplaceAllString(stuff, `\$1`)
				// A leading multi-byte rune's first byte is never one of these, which
				// is why comparing bytes says the same thing as CPython comparing
				// characters.
				switch stuff[0] {
				case '!':
					stuff = "^" + stuff[1:]
				case '^', '[':
					// Only in first position: a '^' there would negate the class and a
					// '[' there would open a POSIX class name.
					stuff = `\` + stuff
				}
				b.WriteString("[" + stuff + "]")
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString(`)\z`)
	return b.String(), never
}

// fnClassBody renders pat[lo:hi] as the inside of a regex character class.
//
// The hyphen handling is where all the subtlety is. A class with no hyphen in it
// is copied out with only its backslashes escaped. A class with one is split into
// chunks at each hyphen -- with a three-character stride, so the hyphen of a
// range and the range it forms are consumed together -- and then rejoined with
// hyphens. That leaves exactly the boundary hyphens as range operators and makes
// every hyphen inside a chunk a literal, which is what turns the `--` of a
// set-difference attempt into a hyphen and a range rather than an operator.
func fnClassBody(pat []rune, lo, hi int) string {
	if !containsRuneIn(pat, '-', lo, hi) {
		return strings.ReplaceAll(string(pat[lo:hi]), `\`, `\\`)
	}

	chunks := []string{}
	k := lo + 1
	if pat[lo] == '!' {
		k = lo + 2
	}
	for {
		h := indexRuneIn(pat, '-', k, hi)
		if h < 0 {
			break
		}
		chunks = append(chunks, string(pat[lo:h]))
		lo = h + 1
		k = h + 3
	}
	switch chunk := string(pat[lo:hi]); {
	case chunk != "":
		chunks = append(chunks, chunk)
	case len(chunks) > 0:
		// A class ending in a hyphen hands it to the previous chunk, where it
		// becomes a literal instead of half a range.
		//
		// There is no third case. An empty final chunk means the last hyphen sat
		// at hi-1, and finding a hyphen is the one thing that appends a chunk, so
		// by then chunks cannot be empty.
		chunks[len(chunks)-1] += "-"
	}

	// Drop the empty ranges. [b-a] is a regex syntax error rather than a class
	// that matches nothing, so the two ends are merged into one chunk -- dropping
	// the low end and the high end, which is what makes [a--b] the single
	// character 'b'.
	for m := len(chunks) - 1; m > 0; m-- {
		if lastRune(chunks[m-1]) > firstRune(chunks[m]) {
			chunks[m-1] = dropLastRune(chunks[m-1]) + dropFirstRune(chunks[m])
			chunks = append(chunks[:m], chunks[m+1:]...)
		}
	}

	for m, s := range chunks {
		s = strings.ReplaceAll(s, `\`, `\\`)
		chunks[m] = strings.ReplaceAll(s, "-", `\-`)
	}
	return strings.Join(chunks, "-")
}

// containsRuneIn reports whether r occurs in pat[lo:hi].
func containsRuneIn(pat []rune, r rune, lo, hi int) bool {
	return indexRuneIn(pat, r, lo, hi) >= 0
}

// indexRuneIn is str.find(r, lo, hi) over a rune slice: the index of the first r
// at or after lo and before hi, or -1. lo past hi finds nothing rather than
// panicking, which is what the chunking loop's three-character stride relies on
// and what Python's bounded find does. hi is always within the slice here, so
// there is no clamp to go untested.
func indexRuneIn(pat []rune, r rune, lo, hi int) int {
	for i := lo; i < hi; i++ {
		if pat[i] == r {
			return i
		}
	}
	return -1
}

// The four rune accessors below all assume a non-empty string, which every chunk
// they see is: a chunk is built from at least one character, and the one merge
// that can empty a chunk empties only the first, which is never read again.

func firstRune(s string) rune {
	r, _ := utf8.DecodeRuneInString(s)
	return r
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
}

func dropFirstRune(s string) string {
	_, size := utf8.DecodeRuneInString(s)
	return s[size:]
}

func dropLastRune(s string) string {
	_, size := utf8.DecodeLastRuneInString(s)
	return s[:len(s)-size]
}
