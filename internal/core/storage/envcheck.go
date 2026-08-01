// Package storage ports src/fleetfix/modules/storage.
package storage

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// EnvIssue is one complaint about one line, or about the file as a whole when
// LineNo is zero.
//
// The JSON names are v1's dataclass field names, because the differential oracle
// serialises the Python dataclass by field name and a rename here would show up
// as every dotenv case diverging.
type EnvIssue struct {
	LineNo  int    `json:"line_no"`
	Raw     string `json:"raw"`
	Message string `json:"message"`
}

// EnvCheckResult is what CheckEnvFile found.
//
// Exists and Readable are separate because they fail for different reasons and
// want different advice: a missing file is usually a deploy that has not run,
// while a present unreadable one is usually a mode or an ownership mistake.
type EnvCheckResult struct {
	Path            string            `json:"path"`
	Exists          bool              `json:"exists"`
	Readable        bool              `json:"readable"`
	Keys            map[string]string `json:"keys"`
	MissingRequired []string          `json:"missing_required"`
	Issues          []EnvIssue        `json:"issues"`
}

// Ok reports whether the file was read and had nothing wrong with it.
//
// A derived accessor, matching v1's @property, so it is deliberately not a field:
// the differential oracle serialises fields and would otherwise compare a value
// neither implementation stores.
func (r EnvCheckResult) Ok() bool {
	return r.Exists && r.Readable && len(r.MissingRequired) == 0 && len(r.Issues) == 0
}

// envLineRE is v1's _LINE_RE with \s widened to what Python's \s actually is.
//
// The key is ASCII by construction, which is why a key like "FÖO" is reported as
// malformed rather than accepted -- v1 behaviour, and the reason the character
// classes here are left as literal ranges instead of \w.
var envLineRE = regexp.MustCompile(
	`^` + pytext.Space + `*(?:export` + pytext.Space + `+)?([A-Za-z_][A-Za-z0-9_]*)` + pytext.Space + `*=(.*)$`,
)

// errNotUTF8 stands in for the UnicodeDecodeError that Path.read_text raises.
//
// Go's file read has no opinion about encoding, so the check is explicit. It has
// to stay a whole-file check rather than a per-line one: v1 decodes before it
// splits, so one bad byte on the last line makes the entire file unreadable and
// none of the keys above it are reported.
var errNotUTF8 = errors.New("invalid or incomplete UTF-8")

// CheckEnvFile reads a dotenv-style file and reports what is wrong with it.
//
// Takes a real path rather than an fs.FS, unlike the /proc and /sys readers. Those
// need the seam because their input is synthetic and has to be faked; this one is
// pointed at a file an operator named, and the path it was given is part of the
// result it returns. A tempdir is the honest seam for it.
//
// requiredKeys may be nil, and its order and its duplicates both survive into
// MissingRequired -- v1 filters the caller's list rather than building a set from
// it, so asking twice for the same missing key is reported twice.
//
// One departure from v1, in the prose only: when the file cannot be read the
// issue's message is Go's error text where v1 interpolates the Python exception.
// The shape, the line number and the "unable to read file: " prefix are the same.
// Reproducing CPython's OSError strings would mean shipping a table of errno
// spellings to make a message that is only ever read by a human.
func CheckEnvFile(path string, requiredKeys []string) EnvCheckResult {
	result := EnvCheckResult{
		Path:            path,
		Keys:            map[string]string{},
		MissingRequired: []string{},
		Issues:          []EnvIssue{},
	}

	// Path.exists() follows symlinks and swallows its own errors, so a broken
	// symlink and an unsearchable parent both read as absent -- and absent is the
	// one branch that reports the required keys as missing.
	if _, err := os.Stat(path); err != nil {
		result.MissingRequired = append(result.MissingRequired, requiredKeys...)
		return result
	}
	result.Exists = true

	data, err := os.ReadFile(path) //nolint:gosec // the path is the argument; naming a file is the whole call
	if err == nil && !utf8.Valid(data) {
		err = errNotUTF8
	}
	if err != nil {
		result.Issues = append(result.Issues, EnvIssue{Message: "unable to read file: " + err.Error()})
		return result
	}
	result.Readable = true

	for i, raw := range pytext.SplitLines(string(data)) {
		// A "#" opens a comment anywhere on the line, including inside what looks
		// like a quoted value: v1 cuts the comment off before it parses, so
		// FOO="a#b" is the key FOO with the value `"a` and no complaint.
		stripped := strings.TrimRightFunc(cutComment(raw), pytext.IsSpace)
		if strings.TrimFunc(stripped, pytext.IsSpace) == "" {
			continue
		}
		match := envLineRE.FindStringSubmatch(stripped)
		if match == nil {
			// The original line, not the comment-stripped one. What the operator
			// has to go and edit is what they wrote.
			result.Issues = append(result.Issues, EnvIssue{
				LineNo:  i + 1,
				Raw:     raw,
				Message: "not in KEY=value form",
			})
			continue
		}
		key := match[1]
		if _, dup := result.Keys[key]; dup {
			result.Issues = append(result.Issues, EnvIssue{
				LineNo:  i + 1,
				Raw:     raw,
				Message: "duplicate key: " + key,
			})
		}
		// The duplicate is reported and then applied anyway, so the value a
		// consumer of this file would see is the value reported here.
		result.Keys[key] = stripQuotes(strings.TrimFunc(match[2], pytext.IsSpace))
	}

	for _, key := range requiredKeys {
		if _, ok := result.Keys[key]; !ok {
			result.MissingRequired = append(result.MissingRequired, key)
		}
	}
	return result
}

// cutComment returns everything before the first "#", which is str.split("#", 1)[0].
func cutComment(line string) string {
	if i := strings.Index(line, "#"); i >= 0 {
		return line[:i]
	}
	return line
}

// stripQuotes removes one matching pair of surrounding quotes.
//
// Byte indexing is exactly Python's rune indexing here: the test requires the
// first and last positions to hold the same character and that character to be
// "'" or `"`, both of which are single-byte in UTF-8 and cannot appear as a
// continuation byte. So a two-byte string that passes cannot be one rune.
//
// Only a matching pair goes, which is why FOO='bar" keeps both of its quotes and
// FOO=' keeps its one.
func stripQuotes(value string) string {
	if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
		return value[1 : len(value)-1]
	}
	return value
}
