package pytext

import (
	"slices"
	"strings"
	"testing"
	"unicode"
)

// Every expectation in this file was captured from CPython 3.10 rather than
// reasoned about, because the whole value of the package is that it matches an
// implementation neither reading the Go nor reading the Python docs would predict.
// If one of these ever needs changing, change it by running the snippet in the
// failing case's comment against python3, not by adjusting it until Go passes.

func TestSplitLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty yields nothing at all", "", []string{}},
		{"no boundary", "a", []string{"a"}},
		{"lf", "a\nb", []string{"a", "b"}},
		// The one that matters most: command output ends in a newline, and a
		// phantom trailing "" would reach every `if not line: continue` guard.
		{"trailing lf drops the empty element", "a\nb\n", []string{"a", "b"}},
		{"crlf is a single boundary", "a\r\nb", []string{"a", "b"}},
		{"bare cr", "a\rb", []string{"a", "b"}},
		{"blank line is preserved", "a\n\nb", []string{"a", "", "b"}},
		{"blank crlf line is preserved", "a\r\n\r\nb", []string{"a", "", "b"}},
		{"lone boundary yields one empty", "\n", []string{""}},
		{"lf then cr are two boundaries", "a\n\r", []string{"a", ""}},
		{"vt", "a\vb", []string{"a", "b"}},
		{"ff", "a\fb", []string{"a", "b"}},
		{"fs and gs", "a\x1cb\x1dc", []string{"a", "b", "c"}},
		{"rs", "a\x1eb", []string{"a", "b"}},
		{"nel", "a\u0085b", []string{"a", "b"}},
		{"line separator", "a\u2028b", []string{"a", "b"}},
		{"paragraph separator", "a\u2029b", []string{"a", "b"}},
		// Whitespace, but not a line boundary -- the pair that would be wrong if
		// the boundary set were derived from isspace.
		{"nbsp is not a boundary", "a\u00a0b", []string{"a\u00a0b"}},
		{"space is not a boundary", "a b", []string{"a b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitLines(tt.in); !slices.Equal(got, tt.want) {
				t.Errorf("SplitLines(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// strings.Split is the port that looks obviously right and is not. Pinning the
// difference stops anyone from "simplifying" this package away.
func TestSplitLinesDiffersFromStringsSplit(t *testing.T) {
	const in = "a\nb\n"
	if got := strings.Split(in, "\n"); len(got) != 3 {
		t.Fatalf("premise changed: strings.Split(%q) = %q", in, got)
	}
	if got := SplitLines(in); len(got) != 2 {
		t.Errorf("SplitLines(%q) = %q, want two elements", in, got)
	}
}

func TestSplitN(t *testing.T) {
	tests := []struct {
		name     string
		in       string
		maxsplit int
		want     []string
	}{
		{"unlimited", "a b c", -1, []string{"a", "b", "c"}},
		{"runs collapse and edges are trimmed", "  a  b  ", -1, []string{"a", "b"}},
		{"tabs and newlines are separators too", "a\tb\nc", -1, []string{"a", "b", "c"}},
		{"empty", "", -1, []string{}},
		{"whitespace only", "   ", -1, []string{}},
		// The four code points Go's unicode.IsSpace omits.
		{"fs", "a\x1cb", -1, []string{"a", "b"}},
		{"gs", "a\x1db", -1, []string{"a", "b"}},
		{"rs", "a\x1eb", -1, []string{"a", "b"}},
		{"us", "a\x1fb", -1, []string{"a", "b"}},
		{"nbsp", "a\u00a0b", -1, []string{"a", "b"}},
		// The remainder keeps its trailing whitespace but not its leading run.
		// This is what carries a df mount point containing spaces intact.
		{"remainder keeps trailing space", "a b c  ", 1, []string{"a", "b c  "}},
		{"remainder loses leading space", "   a b c", 1, []string{"a", "b c"}},
		{"maxsplit above the field count", "a b", 5, []string{"a", "b"}},
		{"maxsplit zero returns the stripped whole", "a b c", 0, []string{"a b c"}},
		{"maxsplit zero strips the leading run", "  a b", 0, []string{"a b"}},
		{"maxsplit zero on empty", "", 0, []string{}},
		{"maxsplit zero on whitespace only", " ", 0, []string{}},
		{"maxsplit zero on one field", "a", 0, []string{"a"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := SplitN(tt.in, tt.maxsplit); !slices.Equal(got, tt.want) {
				t.Errorf("SplitN(%q, %d) = %q, want %q", tt.in, tt.maxsplit, got, tt.want)
			}
		})
	}
}

func TestFieldsIsUnlimitedSplitN(t *testing.T) {
	const in = "  a\x1cb   c "
	if got, want := Fields(in), SplitN(in, -1); !slices.Equal(got, want) {
		t.Errorf("Fields(%q) = %q, want %q", in, got, want)
	}
}

// strings.Fields is the near-miss here: identical on every fixture in the corpus,
// different on the four separators. A test that only used ordinary input would
// pass with strings.Fields substituted in, which is the whole risk.
func TestFieldsDiffersFromStringsFields(t *testing.T) {
	const in = "a\x1cb"
	if got := strings.Fields(in); len(got) != 1 {
		t.Fatalf("premise changed: strings.Fields(%q) = %q", in, got)
	}
	if got := Fields(in); len(got) != 2 {
		t.Errorf("Fields(%q) = %q, want two fields", in, got)
	}
}

func TestIsSpaceIsUnicodePlusTheFourSeparators(t *testing.T) {
	// The measured delta, asserted as a closed set: exactly these four are
	// whitespace to Python and not to Go, and nothing else differs in either
	// direction across the whole code point space.
	for r := rune(0); r <= 0x10FFFF; r++ {
		extra := r >= 0x1C && r <= 0x1F
		if want := unicode.IsSpace(r) || extra; IsSpace(r) != want {
			t.Fatalf("IsSpace(%U) = %v, want %v", r, IsSpace(r), want)
		}
	}
	for r := rune(0x1C); r <= 0x1F; r++ {
		if unicode.IsSpace(r) {
			t.Errorf("premise changed: unicode.IsSpace(%U) is now true", r)
		}
	}
}

// Invariants that hold for any input, checked against generated garbage the tables
// above would never contain.
func FuzzSplitN(f *testing.F) {
	for _, s := range []string{"", " ", "a b c", "a\x1cb", "  a  b  ", "a\nb\r\nc"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		all := Fields(s)
		for i, field := range all {
			if field == "" {
				t.Fatalf("Fields(%q)[%d] is empty", s, i)
			}
			if strings.IndexFunc(field, IsSpace) >= 0 {
				t.Fatalf("Fields(%q)[%d] = %q contains whitespace", s, i, field)
			}
		}
		// A limited split yields the same leading fields as an unlimited one, plus
		// at most one remainder. Anything else means maxsplit changed the scan
		// rather than just stopping it.
		for n := range 4 {
			got := SplitN(s, n)
			if len(got) > n+1 {
				t.Fatalf("SplitN(%q, %d) returned %d elements", s, n, len(got))
			}
			if len(got) > len(all) {
				t.Fatalf("SplitN(%q, %d) = %q has more elements than Fields = %q", s, n, got, all)
			}
			for i := 0; i < len(got)-1; i++ {
				if got[i] != all[i] {
					t.Fatalf("SplitN(%q, %d)[%d] = %q, want %q", s, n, i, got[i], all[i])
				}
			}
		}
	})
}

// SplitLines loses only the boundaries: every element must be a substring of the
// input, and the elements must appear in order without overlapping.
func FuzzSplitLines(f *testing.F) {
	for _, s := range []string{"", "\n", "a\r\nb", "a\n\nb\n", "a\u2028b", "a\u00a0b"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		at := 0
		for i, line := range SplitLines(s) {
			if strings.IndexFunc(line, isLineBoundary) >= 0 {
				t.Fatalf("SplitLines(%q)[%d] = %q contains a boundary", s, i, line)
			}
			idx := strings.Index(s[at:], line)
			if idx < 0 {
				t.Fatalf("SplitLines(%q)[%d] = %q is not a substring at or after %d", s, i, line, at)
			}
			at += idx + len(line)
		}
	})
}
