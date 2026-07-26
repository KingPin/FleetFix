package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Case is one row of testdata/cases.jsonl, the manifest both oracles read.
//
// The manifest is the contract between the implementations: it names a
// language-neutral function, the fixture to feed it, and the arguments that are
// not in the fixture. Neither side may skip a case it does not like.
type Case struct {
	ID      string         `json:"id"`
	Fn      string         `json:"fn"`
	Fixture string         `json:"fixture"`
	SHA256  string         `json:"sha256"`
	Input   string         `json:"input"`
	Args    map[string]any `json:"args"`
}

// recordError is the error form of a result. Only the code is compared: Python's
// message prose cannot be reproduced in Go, so comparing it would generate
// divergences that can never be fixed.
type recordError struct {
	Code string `json:"code"`
	Fn   string `json:"fn,omitempty"`
}

func runOracle(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ffdiff oracle", flag.ContinueOnError)
	fs.SetOutput(stderr)
	manifest := fs.String("manifest", filepath.Join("testdata", "cases.jsonl"), "case manifest to run")
	testdata := fs.String("testdata", "testdata", "directory the manifest's fixture paths are relative to")
	out := fs.String("out", "", "write here instead of stdout")
	listFunctions := fs.Bool("list-functions", false, "print the dispatchable function names and exit")
	var only caseList
	fs.Var(&only, "case", "run only this id (repeatable)")
	if err := fs.Parse(argv); err != nil {
		return err
	}

	if *listFunctions {
		names := make([]string, 0, len(dispatch))
		for name := range dispatch {
			names = append(names, name)
		}
		sort.Strings(names)
		_, err := fmt.Fprintln(stdout, strings.Join(names, "\n"))
		return err
	}

	cases, err := loadCases(*manifest)
	if err != nil {
		return err
	}
	if len(only) > 0 {
		cases, err = only.filter(cases)
		if err != nil {
			return err
		}
	}

	// One temp directory for the whole run, holding the copies that input="path"
	// cases are handed. Writing them out rather than passing the fixture itself
	// keeps a parser that opens its input for writing from mutating the corpus.
	tmp, err := os.MkdirTemp("", "ffdiff-oracle-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var buf strings.Builder
	for _, c := range cases {
		rec, err := runCase(c, *testdata, tmp)
		if err != nil {
			return fmt.Errorf("case %s: %w", c.ID, err)
		}
		line, err := marshalRecord(rec)
		if err != nil {
			return fmt.Errorf("case %s: %w", c.ID, err)
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}

	if *out == "" {
		_, err := io.WriteString(stdout, buf.String())
		return err
	}
	return os.WriteFile(*out, []byte(buf.String()), 0o600)
}

// runCase produces the record for one case: {"id":…, "value":…} or
// {"id":…, "error":{"code":…}}. A case that fails is a result to record, not a
// reason to abort the batch -- the Python side records the exception class the
// same way, and the comparison is what decides whether the two agree.
//
// A fixture that is missing, unreadable, or does not match its manifest hash is
// the exception: that is corpus corruption rather than a result, and it aborts
// the run. Recording it as an error code would let a mangled fixture read as a
// divergence in the parser, and a divergence is something a reader goes looking
// for in the wrong place.
func runCase(c Case, testdataDir, tmpDir string) (map[string]any, error) {
	data, err := os.ReadFile(filepath.Join(testdataDir, filepath.FromSlash(c.Fixture)))
	if err != nil {
		return nil, err
	}
	if got := hex.EncodeToString(hashOf(data)); got != c.SHA256 {
		return nil, fmt.Errorf("fixture %s hashes to %s, manifest says %s", c.Fixture, got, c.SHA256)
	}

	ad, ok := dispatch[c.Fn]
	if !ok {
		return errorRecord(c.ID, recordError{Code: "UnknownFunction", Fn: c.Fn}), nil
	}
	if ad.input != c.Input {
		return nil, fmt.Errorf("manifest says input=%q for %s, adapter takes %q", c.Input, c.Fn, ad.input)
	}

	payload := string(data)
	needle := ""
	switch c.Input {
	case "path":
		// Keep the fixture's own basename: a reader debugging a failure can tell
		// which capture a temp path came from.
		target := filepath.Join(tmpDir, filepath.Base(filepath.FromSlash(c.Fixture)))
		if err := os.WriteFile(target, data, 0o600); err != nil {
			return nil, err
		}
		payload, needle = target, target
	case "tree":
		// A real directory rather than an fstest.MapFS, so both oracles hand their
		// implementation the same thing: os.DirFS over a materialised tree answers
		// the same questions Python's Path does, including the ones a synthesised
		// filesystem gets subtly wrong -- a file where a directory was expected
		// reports ENOTDIR, not "no such file".
		//
		// One directory per case, not per fixture: two cases sharing a tree must not
		// be able to see each other's writes, even though nothing here writes today.
		root := filepath.Join(tmpDir, strings.NewReplacer("/", "_", "#", "-").Replace(c.ID))
		if err := materialiseTree(data, root); err != nil {
			return nil, fmt.Errorf("fixture %s: %w", c.Fixture, err)
		}
		payload, needle = root, root
	}

	value, err := ad.run(payload, c.Args)
	if err != nil {
		return errorRecord(c.ID, recordError{Code: errorCode(err)}), nil
	}
	plain, err := canonical(value)
	if err != nil {
		return nil, fmt.Errorf("%s returned a value that will not encode: %w", c.Fn, err)
	}
	if needle != "" {
		plain = redact(plain, needle)
	}
	return map[string]any{"id": c.ID, "value": plain}, nil
}

// materialiseTree writes a JSON directory description out as a real tree: a string
// is a file's contents, a nested object is a directory.
//
// The fixture kind the readers that walk a directory need -- /sys/class/thermal,
// /sys/class/net, /proc/<pid> -- where one checked-in JSON file is a whole captured
// tree and so keeps a checksum in the manifest like every other fixture. An empty
// object is an empty directory, which is a real sysfs shape: a driver that
// registered and then failed leaves one behind, and the reader must skip it for
// that reason rather than because it was not there at all.
func materialiseTree(data []byte, root string) error {
	var node map[string]any
	if err := json.Unmarshal(data, &node); err != nil {
		return err
	}
	return writeTree(node, root)
}

func writeTree(node map[string]any, at string) error {
	if err := os.MkdirAll(at, 0o700); err != nil {
		return err
	}
	for name, child := range node {
		target := filepath.Join(at, name)
		switch v := child.(type) {
		case string:
			if err := os.WriteFile(target, []byte(v), 0o600); err != nil {
				return err
			}
		case map[string]any:
			if err := writeTree(v, target); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%s is %T, want a string (a file) or an object (a directory)", name, child)
		}
	}
	return nil
}

func errorRecord(id string, e recordError) map[string]any {
	return map[string]any{"id": id, "error": e}
}

// canonical reduces a result to plain JSON types, the same shape py_oracle's
// plain() produces for the corresponding dataclass. Going through the encoder
// rather than reflecting over the struct means the json tags -- the same tags
// the report envelope ships with -- are what defines the field names, so a tag
// that drifts from the Python field name shows up as a divergence rather than
// being quietly papered over here.
func canonical(v any) (any, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return decodeValue(b)
}

// redact replaces the temp input path wherever it surfaced in a result, matching
// py_oracle. A result that carries the path it was handed would otherwise differ
// on every run for a reason that has nothing to do with parsing.
func redact(value any, needle string) any {
	switch v := value.(type) {
	case string:
		if v == needle {
			return "<input-path>"
		}
		return v
	case map[string]any:
		for k, elem := range v {
			v[k] = redact(elem, needle)
		}
		return v
	case []any:
		for i, elem := range v {
			v[i] = redact(elem, needle)
		}
		return v
	default:
		return value
	}
}

// errorCode reports the CPython exception class name the Python implementation
// raises for this failure. An adapter says so by returning a pyError; anything
// else is a bug in the Go port rather than a reproduction of Python behaviour,
// and gets a code that cannot be mistaken for one.
func errorCode(err error) string {
	var pe pyError
	if errors.As(err, &pe) {
		return pe.Code
	}
	return "GoError"
}

// pyError carries the exception class name Python raises where Go returns an
// error, so the two oracles can be compared on codes instead of prose.
type pyError struct{ Code string }

func (e pyError) Error() string { return "python raises " + e.Code }

func hashOf(b []byte) []byte {
	sum := sha256.Sum256(b)
	return sum[:]
}

// marshalRecord writes the record with sorted keys and no HTML escaping. Neither
// matters to the comparison, which decodes both sides first; both matter to the
// human reading a failing case out of the two .jsonl files side by side.
func marshalRecord(rec map[string]any) ([]byte, error) {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(buf.String(), "\n")), nil
}

func loadCases(path string) ([]Case, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cases []Case
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var c Case
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		cases = append(cases, c)
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("%s holds no cases", path)
	}
	return cases, nil
}

// caseList is a repeatable --case flag.
type caseList []string

func (c *caseList) String() string { return strings.Join(*c, ",") }

func (c *caseList) Set(v string) error {
	*c = append(*c, v)
	return nil
}

func (c *caseList) filter(cases []Case) ([]Case, error) {
	wanted := make(map[string]bool, len(*c))
	for _, id := range *c {
		wanted[id] = true
	}
	kept := make([]Case, 0, len(wanted))
	for _, cs := range cases {
		if wanted[cs.ID] {
			kept = append(kept, cs)
			delete(wanted, cs.ID)
		}
	}
	if len(wanted) > 0 {
		missing := make([]string, 0, len(wanted))
		for id := range wanted {
			missing = append(missing, id)
		}
		sort.Strings(missing)
		return nil, fmt.Errorf("no such case: %s", strings.Join(missing, ", "))
	}
	return kept, nil
}
