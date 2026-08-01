package storage

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
	"github.com/google/go-cmp/cmp"
)

// The line breaks and the whitespace runes that are Python's but not ASCII's.
// Escapes rather than glyphs: every one of them either moves the cursor or is
// blank, so written inline they would be invisible in the diff that added them.
const (
	verticalTab   = "\v"
	formFeed      = "\f"
	fileSeparator = "\x1c"
	nextLine      = "\u0085" // NEL
	lineSeparator = "\u2028"
	nbsp          = "\u00a0" // NO-BREAK SPACE: not a line break, but is \s
	bom           = "\ufeff" // BYTE ORDER MARK: decodes fine, then fails the key regex
)

// write puts body at <tempdir>/e.env and hands back the path.
//
// A fresh directory per case rather than a fresh name in a shared one, so a case
// that chmods or symlinks cannot reach another case's file.
func write(t *testing.T, body []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "e.env")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCheckEnvFileFixtures(t *testing.T) {
	tests := []struct {
		fixture  string
		required []string
		want     EnvCheckResult
	}{
		{
			fixture: "dotenv/well_formed.txt",
			// The three the v1 test asks for; DEBUG is parsed but not required.
			required: []string{"DB_URL", "API_KEY", "PORT"},
			want: EnvCheckResult{
				Keys: map[string]string{
					"DB_URL":  "postgres://localhost",
					"API_KEY": "s3cret",
					"DEBUG":   "true",
					"PORT":    "5432",
				},
			},
		},
		{
			fixture:  "dotenv/single_key.txt",
			required: []string{"FOO", "MISSING"},
			want: EnvCheckResult{
				Keys:            map[string]string{"FOO": "bar"},
				MissingRequired: []string{"MISSING"},
			},
		},
		{
			fixture: "dotenv/empty_value.txt",
			want:    EnvCheckResult{Keys: map[string]string{"EMPTY": ""}},
		},
		{
			fixture: "dotenv/inline_comment.txt",
			want:    EnvCheckResult{Keys: map[string]string{"KEY": "value"}},
		},
		{
			fixture: "dotenv/duplicate_keys.txt",
			want: EnvCheckResult{
				Keys: map[string]string{"X": "2"},
				Issues: []EnvIssue{
					{LineNo: 2, Raw: "X=2", Message: "duplicate key: X"},
				},
			},
		},
		{
			fixture: "dotenv/malformed_line.txt",
			want: EnvCheckResult{
				Keys: map[string]string{"GOOD": "ok", "ALSO_GOOD": "also-ok"},
				Issues: []EnvIssue{
					{LineNo: 2, Raw: "this line is garbage", Message: "not in KEY=value form"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.fixture, func(t *testing.T) {
			// The fixtures are read where they lie. CheckEnvFile only reads, and a
			// copy into a tempdir would only prove that the copy parsed.
			path := fixture.Path(tt.fixture)
			want := tt.want
			want.Path, want.Exists, want.Readable = path, true, true
			assertResult(t, CheckEnvFile(path, tt.required), want)
		})
	}
}

// TestCheckEnvFile is the measured table: every expectation here is what CPython
// 3.14.6's check_env_file returned for a file holding the same bytes.
func TestCheckEnvFile(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		required []string
		keys     map[string]string
		missing  []string
		issues   []EnvIssue
	}{
		{name: "empty"},
		{name: "blank lines only", body: "\n\n   \n\t\n"},
		{name: "simple", body: "FOO=bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "no trailing newline", body: "FOO=bar", keys: map[string]string{"FOO": "bar"}},
		{name: "crlf", body: "FOO=bar\r\n", keys: map[string]string{"FOO": "bar"}},

		// splitlines() breaks on all of these, so each is two lines, not one
		// malformed one. A parser written against strings.Split(s, "\n") sees the
		// opposite and reports a bogus issue.
		{name: "cr only", body: "A=1\rB=2\r", keys: map[string]string{"A": "1", "B": "2"}},
		{name: "vertical tab", body: "A=1" + verticalTab + "B=2", keys: map[string]string{"A": "1", "B": "2"}},
		{name: "form feed", body: "A=1" + formFeed + "B=2", keys: map[string]string{"A": "1", "B": "2"}},
		{name: "file separator", body: "A=1" + fileSeparator + "B=2", keys: map[string]string{"A": "1", "B": "2"}},
		{name: "next line", body: "A=1" + nextLine + "B=2", keys: map[string]string{"A": "1", "B": "2"}},
		{name: "line separator", body: "A=1" + lineSeparator + "B=2", keys: map[string]string{"A": "1", "B": "2"}},

		{name: "full line comment", body: "# hi\nFOO=bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "indented comment", body: "   # hi\nFOO=bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "inline comment", body: "FOO=bar  # note\n", keys: map[string]string{"FOO": "bar"}},
		// The comment is cut before the value is parsed, so a "#" inside quotes ends
		// the value and takes the closing quote with it. Surprising, and v1's.
		{name: "hash inside value", body: "FOO=\"a#b\"\n", keys: map[string]string{"FOO": `"a`}},
		{name: "hash after equals", body: "FOO=#comment\n", keys: map[string]string{"FOO": ""}},
		{
			name: "hash in key position", body: "FO#O=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "FO#O=bar", Message: "not in KEY=value form"}},
		},
		{name: "bare hash line", body: "#\n"},

		// "export" is matched literally and lowercase, and needs at least one
		// whitespace rune after it.
		{name: "export", body: "export FOO=bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "export with tab", body: "export\tFOO=bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "export no space", body: "exportFOO=bar\n", keys: map[string]string{"exportFOO": "bar"}},
		{
			name: "EXPORT uppercase", body: "EXPORT FOO=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "EXPORT FOO=bar", Message: "not in KEY=value form"}},
		},
		{
			name: "export twice", body: "export export FOO=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "export export FOO=bar", Message: "not in KEY=value form"}},
		},

		{name: "leading whitespace", body: "   FOO=bar\n", keys: map[string]string{"FOO": "bar"}},
		// NBSP is \s to Python, so it is accepted as indentation where ASCII-only
		// \s would reject the line.
		{name: "nbsp indentation", body: nbsp + "FOO=bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "space before equals", body: "FOO  =bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "space after equals", body: "FOO=  bar\n", keys: map[string]string{"FOO": "bar"}},
		{name: "empty value", body: "FOO=\n", keys: map[string]string{"FOO": ""}},
		{name: "value with equals", body: "FOO=a=b\n", keys: map[string]string{"FOO": "a=b"}},
		{name: "value with spaces", body: "FOO=a b c\n", keys: map[string]string{"FOO": "a b c"}},
		{name: "underscore key", body: "_FOO=bar\n", keys: map[string]string{"_FOO": "bar"}},

		{
			name: "no equals", body: "garbage\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "garbage", Message: "not in KEY=value form"}},
		},
		{
			name: "equals first", body: "=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "=bar", Message: "not in KEY=value form"}},
		},
		{
			name: "digit-leading key", body: "1FOO=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "1FOO=bar", Message: "not in KEY=value form"}},
		},
		{
			name: "dash in key", body: "FO-O=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "FO-O=bar", Message: "not in KEY=value form"}},
		},
		{
			name: "dot in key", body: "FO.O=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "FO.O=bar", Message: "not in KEY=value form"}},
		},
		{
			// The key class is ASCII by construction, so an accented key is
			// malformed rather than accepted -- not \w, which would take it.
			name: "unicode key", body: "FÖO=bar\n",
			issues: []EnvIssue{{LineNo: 1, Raw: "FÖO=bar", Message: "not in KEY=value form"}},
		},
		{
			// The BOM survives decoding as a rune and then sits in front of the key.
			name: "bom", body: bom + "A=1\n",
			issues: []EnvIssue{{LineNo: 1, Raw: bom + "A=1", Message: "not in KEY=value form"}},
		},

		// One matching pair of quotes is removed, and only a matching pair.
		{name: "single quoted", body: "FOO='bar'\n", keys: map[string]string{"FOO": "bar"}},
		{name: "double quoted", body: "FOO=\"bar\"\n", keys: map[string]string{"FOO": "bar"}},
		{name: "mismatched quotes", body: "FOO='bar\"\n", keys: map[string]string{"FOO": `'bar"`}},
		{name: "one quote", body: "FOO='\n", keys: map[string]string{"FOO": "'"}},
		{name: "two quotes only", body: "FOO=''\n", keys: map[string]string{"FOO": ""}},
		{name: "quote inside", body: "FOO=a'b'c\n", keys: map[string]string{"FOO": "a'b'c"}},
		{name: "quoted with inner space", body: "FOO=' bar '\n", keys: map[string]string{"FOO": " bar "}},
		{name: "backtick not a quote", body: "FOO=`bar`\n", keys: map[string]string{"FOO": "`bar`"}},

		// The duplicate is reported and then applied, so Keys holds the last value.
		{
			name: "duplicate", body: "X=1\nX=2\n",
			keys:   map[string]string{"X": "2"},
			issues: []EnvIssue{{LineNo: 2, Raw: "X=2", Message: "duplicate key: X"}},
		},
		{
			name: "triplicate", body: "X=1\nX=2\nX=3\n",
			keys: map[string]string{"X": "3"},
			issues: []EnvIssue{
				{LineNo: 2, Raw: "X=2", Message: "duplicate key: X"},
				{LineNo: 3, Raw: "X=3", Message: "duplicate key: X"},
			},
		},

		{
			name: "required all present", body: "A=1\nB=2\n", required: []string{"A", "B"},
			keys: map[string]string{"A": "1", "B": "2"},
		},
		{
			name: "required one missing", body: "A=1\n", required: []string{"A", "B"},
			keys: map[string]string{"A": "1"}, missing: []string{"B"},
		},
		{
			// The caller's list is filtered, not turned into a set: order survives
			// and so do duplicates, because the operator's list is the report's order.
			name: "required order preserved", body: "\n", required: []string{"Z", "A", "M"},
			missing: []string{"Z", "A", "M"},
		},
		{
			name: "required duplicated", body: "\n", required: []string{"A", "A"},
			missing: []string{"A", "A"},
		},
		{name: "required empty list", body: "A=1\n", required: []string{}, keys: map[string]string{"A": "1"}},
		{name: "required nil", body: "A=1\n", required: nil, keys: map[string]string{"A": "1"}},
		{
			// A malformed line does not contribute its key, so the key is both
			// complained about and reported missing.
			name: "required matches a malformed line's key", body: "A-1\n", required: []string{"A"},
			missing: []string{"A"},
			issues:  []EnvIssue{{LineNo: 1, Raw: "A-1", Message: "not in KEY=value form"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := write(t, []byte(tt.body))
			want := EnvCheckResult{
				Path: path, Exists: true, Readable: true,
				Keys: tt.keys, MissingRequired: tt.missing, Issues: tt.issues,
			}
			assertResult(t, CheckEnvFile(path, tt.required), want)
		})
	}
}

// TestCheckEnvFileAbsent covers the one branch that reports the required keys as
// missing: nothing was read, so nothing can satisfy them.
//
// Path.exists() follows symlinks and swallows its own errors, which is why a
// dangling symlink lands here rather than in the unreadable branch.
func TestCheckEnvFileAbsent(t *testing.T) {
	tests := []struct {
		name     string
		make     func(t *testing.T, path string)
		required []string
	}{
		{name: "absent", make: func(*testing.T, string) {}},
		{name: "absent with required", make: func(*testing.T, string) {}, required: []string{"A", "B"}},
		{
			name: "broken symlink",
			make: func(t *testing.T, path string) {
				if err := os.Symlink(filepath.Join(filepath.Dir(path), "nope"), path); err != nil {
					t.Fatal(err)
				}
			},
			required: []string{"A"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "e.env")
			tt.make(t, path)
			assertResult(t, CheckEnvFile(path, tt.required), EnvCheckResult{
				Path: path, MissingRequired: tt.required,
			})
		})
	}
}

// TestCheckEnvFileUnreadable covers the present-but-unreadable branch, where the
// required keys are deliberately *not* reported missing: the file may well define
// them and we simply cannot tell.
//
// The message prose is the one place this port departs from v1 -- it carries Go's
// error text where Python carried the OSError's -- so these assert the shape and
// the prefix rather than the whole string.
func TestCheckEnvFileUnreadable(t *testing.T) {
	tests := []struct {
		name string
		make func(t *testing.T, path string)
	}{
		{
			name: "directory",
			make: func(t *testing.T, path string) {
				if err := os.Mkdir(path, 0o750); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "invalid utf-8",
			make: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("A=1\nB=\xff\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			// v1 decodes the whole file before it splits, so one bad byte on the
			// last line loses the good keys above it too.
			name: "invalid utf-8 on the last line only",
			make: func(t *testing.T, path string) {
				if err := os.WriteFile(path, []byte("A=1\n\xff"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "unreadable mode",
			make: func(t *testing.T, path string) {
				if os.Getuid() == 0 {
					t.Skip("root reads a 0000 file regardless of its mode")
				}
				if err := os.WriteFile(path, []byte("A=1\n"), 0o000); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "e.env")
			tt.make(t, path)

			got := CheckEnvFile(path, []string{"A"})
			if got.Path != path || !got.Exists || got.Readable {
				t.Errorf("Path/Exists/Readable = %q, %v, %v, want %q, true, false",
					got.Path, got.Exists, got.Readable, path)
			}
			if len(got.Keys) != 0 || len(got.MissingRequired) != 0 {
				t.Errorf("Keys = %v, MissingRequired = %v, want both empty",
					got.Keys, got.MissingRequired)
			}
			if len(got.Issues) != 1 {
				t.Fatalf("Issues = %v, want exactly one", got.Issues)
			}
			// LineNo zero is what says "this is about the file, not about a line".
			if issue := got.Issues[0]; issue.LineNo != 0 || issue.Raw != "" ||
				!strings.HasPrefix(issue.Message, "unable to read file: ") {
				t.Errorf("Issues[0] = %+v, want line 0, no raw, and the read-failure prefix", issue)
			}
			if got.Ok() {
				t.Error("Ok() = true for a file that could not be read")
			}
		})
	}
}

func TestEnvCheckResultOk(t *testing.T) {
	tests := []struct {
		name string
		in   EnvCheckResult
		want bool
	}{
		{"read and clean", EnvCheckResult{Exists: true, Readable: true}, true},
		{"absent", EnvCheckResult{}, false},
		{"present but unreadable", EnvCheckResult{Exists: true}, false},
		{
			"missing a required key",
			EnvCheckResult{Exists: true, Readable: true, MissingRequired: []string{"A"}},
			false,
		},
		{
			"has an issue",
			EnvCheckResult{Exists: true, Readable: true, Issues: []EnvIssue{{LineNo: 1}}},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.Ok(); got != tt.want {
				t.Errorf("Ok() = %v, want %v", got, tt.want)
			}
		})
	}
}

// keyShape is the key the regex can produce, asserted independently of the regex
// so a widened character class shows up as a failure rather than as agreement.
var keyShape = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func FuzzCheckEnvFile(f *testing.F) {
	f.Add("FOO=bar\n", "FOO")
	f.Add("", "")
	f.Add("export A='x'  # c\nA=2\n", "A")
	f.Add("garbage\n"+nbsp+"B=1"+nextLine+"C=", "B")
	f.Add("\xff", "A")
	f.Add(bom+"A=1\n", "")

	f.Fuzz(func(t *testing.T, body, required string) {
		path := filepath.Join(t.TempDir(), "e.env")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got := CheckEnvFile(path, []string{required})

		// The wire structs forbid nil: a JSON null where the Python oracle emits []
		// or {} is a divergence in the contract even when the values agree.
		if got.Keys == nil || got.MissingRequired == nil || got.Issues == nil {
			t.Fatalf("nil map or slice in %+v", got)
		}
		if got.Path != path {
			t.Fatalf("Path = %q, want the path handed in, %q", got.Path, path)
		}
		if !got.Exists {
			t.Fatalf("Exists = false for a file just written")
		}
		if !got.Readable {
			// Unreadable is reachable from a fuzzed body only through bad UTF-8, and
			// it reports one whole-file issue and nothing else.
			if len(got.Keys) != 0 || len(got.MissingRequired) != 0 || len(got.Issues) != 1 {
				t.Fatalf("unreadable result carries more than the read failure: %+v", got)
			}
			return
		}

		lines := len(pytext.SplitLines(body))
		for _, issue := range got.Issues {
			if issue.LineNo < 1 || issue.LineNo > lines {
				t.Fatalf("issue on line %d of a %d-line file: %+v", issue.LineNo, lines, issue)
			}
		}
		for key := range got.Keys {
			if !keyShape.MatchString(key) {
				t.Fatalf("accepted key %q, which is not KEY-shaped", key)
			}
		}
		if _, ok := got.Keys[required]; ok && len(got.MissingRequired) != 0 {
			t.Fatalf("key %q is present and also reported missing: %+v", required, got)
		}
		if want := len(got.MissingRequired) == 0 && len(got.Issues) == 0; got.Ok() != want {
			t.Fatalf("Ok() = %v for %+v", got.Ok(), got)
		}
	})
}

// assertResult compares whole results, treating a nil map or slice in want as the
// empty one CheckEnvFile actually returns -- the table above would otherwise have
// to spell out three empty containers on every one of its clean cases.
func assertResult(t *testing.T, got, want EnvCheckResult) {
	t.Helper()
	if want.Keys == nil {
		want.Keys = map[string]string{}
	}
	if want.MissingRequired == nil {
		want.MissingRequired = []string{}
	}
	if want.Issues == nil {
		want.Issues = []EnvIssue{}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("CheckEnvFile mismatch (-want +got):\n%s", diff)
	}
	if ok := len(want.MissingRequired) == 0 && len(want.Issues) == 0 && want.Exists && want.Readable; got.Ok() != ok {
		t.Errorf("Ok() = %v, want %v", got.Ok(), ok)
	}
}
