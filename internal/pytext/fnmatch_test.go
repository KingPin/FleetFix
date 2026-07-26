package pytext

import (
	"regexp"
	"strings"
	"testing"
)

// Every want below was measured by running fnmatch.fnmatchcase, on CPython 3.10
// and 3.14 both, and the two agree on every one.
var fnMatchCases = []struct {
	name    string
	pattern string
	want    bool
}{
	// The globs v1 ships (modules/storage/stale.py:22,37), against names that do
	// and do not hit.
	{name: "dump.sql", pattern: "*.sql", want: true},
	{name: "dump.SQL", pattern: "*.sql", want: false},
	{name: ".sql", pattern: "*.sql", want: true},
	{name: "sql", pattern: "*.sql", want: false},
	{name: "dump.sql.gz", pattern: "*.sql.gz", want: true},
	{name: "dump.sql.gz", pattern: "*.sql", want: false},
	{name: "db.dump", pattern: "*.dump", want: true},
	{name: "archive.tar.gz", pattern: "*.tar.gz", want: true},
	{name: "site.tgz", pattern: "*.tgz", want: true},
	{name: "app.log.1", pattern: "*.log.[0-9]", want: true},
	{name: "app.log.12", pattern: "*.log.[0-9]", want: false},
	{name: "app.log.12", pattern: "*.log.[0-9][0-9]", want: true},
	{name: "app.log.x", pattern: "*.log.[0-9]", want: false},
	{name: "app.log.gz", pattern: "*.log.gz", want: true},
	{name: "app.log.1.gz", pattern: "*.log.[0-9]*.gz", want: true},
	{name: "app.log.10.gz", pattern: "*.log.[0-9]*.gz", want: true},
	{name: "app.log.1gz", pattern: "*.log.[0-9]*.gz", want: false},
	{name: "app.log.old", pattern: "*.log.old", want: true},
	{name: "app.log.OLD", pattern: "*.log.old", want: false},
	// [0-9] is a code point range, so a digit from another script is not in it --
	// which is the opposite of what Python's \d would have done, and the reason
	// the class is translated rather than reinterpreted.
	{name: "app.log.١", pattern: "*.log.[0-9]", want: false},
	{name: "app.log.１", pattern: "*.log.[0-9]", want: false},

	// Stars. A star crosses a '/' and a newline, unlike path.Match's.
	{name: "", pattern: "*", want: true},
	{name: "", pattern: "", want: true},
	{name: "a", pattern: "", want: false},
	{name: "", pattern: "?", want: false},
	{name: "anything at all", pattern: "*", want: true},
	{name: "a/b/c", pattern: "*", want: true},
	{name: "a/b/c", pattern: "a*c", want: true},
	{name: "a\nb", pattern: "*", want: true},
	{name: "a\nb", pattern: "a?b", want: true},
	{name: "ab", pattern: "**", want: true},
	{name: "ab", pattern: "*a*b*", want: true},
	{name: "ba", pattern: "*a*b*", want: false},
	{name: "aXbYc", pattern: "a*b*c", want: true},
	{name: "abc", pattern: "a*b*c", want: true},
	{name: "ac", pattern: "a*b*c", want: false},

	// '?' is one character, and one character can be four bytes.
	{name: "a", pattern: "?", want: true},
	{name: "ab", pattern: "?", want: false},
	{name: "é", pattern: "?", want: true},
	{name: "\U0001f600", pattern: "?", want: true},
	{name: "éé", pattern: "??", want: true},

	// Classes, including the four characters that mean something to a regex
	// engine in first position and nothing to fnmatch.
	{name: "a", pattern: "[abc]", want: true},
	{name: "d", pattern: "[abc]", want: false},
	{name: "a", pattern: "[!abc]", want: false},
	{name: "d", pattern: "[!abc]", want: true},
	{name: "]", pattern: "[]]", want: true},
	{name: "a", pattern: "[]]", want: false},
	{name: "]", pattern: "[!]]", want: false},
	{name: "a", pattern: "[!]]", want: true},
	{name: "]", pattern: "[!]a]", want: false},
	{name: "a", pattern: "[!]a]", want: false},
	{name: "x", pattern: "[!]a]", want: true},
	{name: "^", pattern: "[^a]", want: true},
	{name: "a", pattern: "[^a]", want: true},
	{name: "x", pattern: "[^a]", want: false},
	{name: "[", pattern: "[[]", want: true},
	{name: "a", pattern: "[[]", want: false},
	{name: "-", pattern: "[-a]", want: true},
	{name: "a", pattern: "[-a]", want: true},
	{name: "-", pattern: "[a-]", want: true},
	{name: "a", pattern: "[a-]", want: true},
	// A backslash is an ordinary class member, not an escape.
	{name: "\\", pattern: "[a\\b]", want: true},
	{name: "a", pattern: "[a\\b]", want: true},
	{name: "b", pattern: "[a\\b]", want: true},
	// The three characters `re` reserves for set operations it has not
	// implemented: members here, in both languages.
	{name: "&", pattern: "[&a]", want: true},
	{name: "~", pattern: "[~a]", want: true},
	{name: "|", pattern: "[|a]", want: true},
	{name: "a", pattern: "[a&&b]", want: true},
	{name: "&", pattern: "[a&&b]", want: true},

	// Empty and inverted ranges. [b-a] is a regex error, so translate collapses
	// it -- to a pattern that matches nothing at all, negated to one that matches
	// any single character, and in [a--b] to the single character 'b'.
	{name: "a", pattern: "[b-a]", want: false},
	{name: "", pattern: "[b-a]", want: false},
	{name: "b", pattern: "[b-a]", want: false},
	{name: "a", pattern: "[!b-a]", want: true},
	{name: "-", pattern: "[!b-a]", want: true},
	{name: "", pattern: "[!b-a]", want: false},
	{name: "ab", pattern: "[!b-a]", want: false},
	{name: "a", pattern: "[a--b]", want: false},
	{name: "b", pattern: "[a--b]", want: true},
	{name: "-", pattern: "[a--b]", want: false},
	// The never-match is the whole pattern's, not just the class's.
	{name: "a", pattern: "x[b-a]y", want: false},
	{name: "xby", pattern: "x[b-a]y", want: false},
	{name: "-", pattern: "[b-a-b]", want: true},
	{name: "b", pattern: "[b-a-b]", want: true},
	{name: "a", pattern: "[b-a-b]", want: false},

	// An unterminated '[' is a literal '[' and the scan resumes at the next
	// character, so nothing between it and the end is skipped.
	{name: "[abc", pattern: "[abc", want: true},
	{name: "a", pattern: "[abc", want: false},
	{name: "[]", pattern: "[]", want: true},
	{name: "[", pattern: "[", want: true},
	{name: "[!", pattern: "[!", want: true},
	{name: "[!]", pattern: "[!]", want: true},
	{name: "a", pattern: "[!]", want: false},
	{name: "x[y", pattern: "x[y", want: true},
	{name: "x[y]z", pattern: "x[y]z", want: false},

	// Ranges that are ranges, and the hyphen that is left over after one.
	{name: "c", pattern: "[a-z]", want: true},
	{name: "C", pattern: "[a-z]", want: false},
	{name: "5", pattern: "[0-9a-f]", want: true},
	{name: "e", pattern: "[0-9a-f]", want: true},
	{name: "g", pattern: "[0-9a-f]", want: false},
	{name: "-", pattern: "[a-c-e]", want: true},
	{name: "d", pattern: "[a-c-e]", want: false},
	{name: "e", pattern: "[a-c-e]", want: true},

	// Multi-byte class members, which is where a byte-wise port of the chunk
	// arithmetic would come apart.
	{name: "ê", pattern: "[é-ê]", want: true},
	{name: "è", pattern: "[é-ê]", want: false},
	{name: "é", pattern: "[ê-é]", want: false},
	{name: "\U0001f600", pattern: "[\U0001f600]", want: true},
	{name: "\U0001f600", pattern: "[\U0001f5ff-\U0001f601]", want: true},
	{name: "é", pattern: "[éê]", want: true},
	{name: "-", pattern: "[ê-é-ë]", want: true},
	{name: "ë", pattern: "[ê-é-ë]", want: true},
	{name: "ê", pattern: "[ê-é-ë]", want: false},

	// Regex metacharacters outside a class are literals.
	{name: "a.b", pattern: "a.b", want: true},
	{name: "axb", pattern: "a.b", want: false},
	{name: "a+b", pattern: "a+b", want: true},
	{name: "ab", pattern: "a+b", want: false},
	{name: "(a)", pattern: "(a)", want: true},
	{name: "a|b", pattern: "a|b", want: true},
	{name: "a", pattern: "a|b", want: false},
	{name: "a$", pattern: "a$", want: true},
	{name: "^a", pattern: "^a", want: true},
	{name: "a\\b", pattern: "a\\b", want: true},
	{name: "ab", pattern: "a\\b", want: false},
	{name: "a b", pattern: "a b", want: true},
	{name: "a\tb", pattern: "a?b", want: true},

	// A match is the whole name. In particular a trailing newline is not
	// forgiven, which is what Python's \Z means and Go's \z, but not Go's '$'.
	{name: "abc", pattern: "a", want: false},
	{name: "abc", pattern: "abc", want: true},
	{name: "abcd", pattern: "abc", want: false},
	{name: "xabc", pattern: "abc", want: false},
	{name: "abc\n", pattern: "abc", want: false},
	{name: "abc\n", pattern: "abc?", want: true},
	{name: "abc\n", pattern: "abc*", want: true},

	// NUL is an ordinary character on both sides.
	{name: "a\x00b", pattern: "a?b", want: true},
	{name: "a\x00b", pattern: "a\x00b", want: true},
}

func TestFNMatchCase(t *testing.T) {
	t.Parallel()

	for _, tc := range fnMatchCases {
		t.Run(tc.pattern+" vs "+tc.name, func(t *testing.T) {
			t.Parallel()
			if got := FNMatchCase(tc.name, tc.pattern); got != tc.want {
				regex, never := fnTranslate(tc.pattern)
				t.Errorf("FNMatchCase(%q, %q) = %v, want %v (regex %q, never=%v)",
					tc.name, tc.pattern, got, tc.want, regex, never)
			}
		})
	}
}

func TestCompileFNPatternAgreesWithTheOneShotDoor(t *testing.T) {
	t.Parallel()

	// The compiled door is the one a directory walk uses, so it gets the same
	// table rather than being taken on trust from the wrapper's one line.
	for _, tc := range fnMatchCases {
		p := CompileFNPattern(tc.pattern)
		if got := p.MatchCase(tc.name); got != tc.want {
			t.Errorf("CompileFNPattern(%q).MatchCase(%q) = %v, want %v",
				tc.pattern, tc.name, got, tc.want)
		}
		// Reusable: a compiled pattern must answer the same way twice.
		if again := p.MatchCase(tc.name); again != tc.want {
			t.Errorf("CompileFNPattern(%q).MatchCase(%q) changed answer on reuse", tc.pattern, tc.name)
		}
	}
}

func TestZeroFNPatternMatchesNothing(t *testing.T) {
	t.Parallel()

	// The zero value is what an all-empty-ranges pattern compiles to, so it has
	// to be safe to call rather than a nil dereference.
	var p FNPattern
	for _, name := range []string{"", "a", "anything"} {
		if p.MatchCase(name) {
			t.Errorf("the zero FNPattern matched %q", name)
		}
	}
}

func TestNewFNPatternRefusesRatherThanPanicsOnARegexItCannotCompile(t *testing.T) {
	t.Parallel()

	// fnTranslate cannot produce this -- FuzzFNTranslateAlwaysCompiles is what
	// keeps that true -- so the guard is reached directly. It exists because the
	// alternative is a MustCompile panic on a glob that came from an operator's
	// YAML file.
	p := newFNPattern(`\A(?s:[)\z`, false)
	if p.MatchCase("[") || p.MatchCase("") {
		t.Error("a pattern that failed to compile matched something")
	}
}

func TestFNTranslateCollapsesRunsOfStars(t *testing.T) {
	t.Parallel()

	// Not a language claim -- the table already pins that "**" and "*" agree.
	// This is the claim that the collapse happens at all, since the only evidence
	// otherwise would be a regex program nobody looks at.
	regex, never := fnTranslate("a***b")
	if never {
		t.Fatalf("fnTranslate(%q) reported never-match", "a***b")
	}
	if strings.Count(regex, ".*") != 1 {
		t.Errorf("fnTranslate(%q) = %q, want exactly one .*", "a***b", regex)
	}
}

func TestFNTranslateReportsNeverForACollapsedRange(t *testing.T) {
	t.Parallel()

	// The flag, not just its effect: CPython spells this as a `(?!)` in the
	// concatenation, RE2 has no lookahead, and a caller of fnTranslate that
	// ignored the flag would compile a regex that matches "" and nothing else.
	if _, never := fnTranslate("[b-a]"); !never {
		t.Errorf("fnTranslate(%q) did not report never-match", "[b-a]")
	}
	if _, never := fnTranslate("[a-b]"); never {
		t.Errorf("fnTranslate(%q) reported never-match", "[a-b]")
	}
}

func FuzzFNTranslateAlwaysCompiles(f *testing.F) {
	for _, tc := range fnMatchCases {
		f.Add(tc.pattern, tc.name)
	}
	// Shapes the table reaches only in combination, seeded so the fuzzer starts
	// from them rather than having to rediscover the bracket arithmetic.
	for _, pattern := range []string{"[", "[]", "[!", "[-", "[--", "[---", "[a-", "[!-]", "[b-a-a-b]", "[\\\\-]", "*[*]*"} {
		f.Add(pattern, "-")
	}

	f.Fuzz(func(t *testing.T, pattern, name string) {
		regex, never := fnTranslate(pattern)
		if !never {
			// The claim that makes newFNPattern's error branch unreachable and
			// CompileFNPattern's missing error return honest.
			if _, err := regexp.Compile(regex); err != nil {
				t.Fatalf("fnTranslate(%q) = %q, which does not compile: %v", pattern, regex, err)
			}
		}

		got := FNMatchCase(name, pattern)
		if never && got {
			t.Fatalf("FNMatchCase(%q, %q) matched a pattern reported as never-matching", name, pattern)
		}
		if got := CompileFNPattern(pattern).MatchCase(name); got != FNMatchCase(name, pattern) {
			t.Fatalf("the compiled and one-shot doors disagree on (%q, %q)", name, pattern)
		}

		// A star matches everything, and a pattern of nothing but literals matches
		// exactly itself. Those two are the whole of what fnmatch promises without
		// having to reimplement it here to check.
		if !FNMatchCase(name, "*") {
			t.Fatalf("FNMatchCase(%q, \"*\") = false", name)
		}
		if !strings.ContainsAny(name, "*?[") && !FNMatchCase(name, name) {
			t.Fatalf("FNMatchCase(%q, %q) = false, but the pattern is all literals", name, name)
		}
	})
}
