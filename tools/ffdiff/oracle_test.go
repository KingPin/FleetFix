package main

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

func TestRunDispatchesModes(t *testing.T) {
	tests := []struct {
		name    string
		argv    []string
		wantErr string
	}{
		{"no mode", nil, "no mode given"},
		{"unknown mode", []string{"diff"}, `unknown mode "diff"`},
		{"help", []string{"--help"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr strings.Builder
			err := run(tt.argv, &stdout, &stderr)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("run(%v) = %v, want nil", tt.argv, err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("run(%v) = %v, want an error mentioning %q", tt.argv, err, tt.wantErr)
			}
			// Either way the caller is told what the modes are.
			if !strings.Contains(stdout.String()+stderr.String(), "oracle") {
				t.Error("no usage text was written")
			}
		})
	}
}

// The oracle is exercised against the checked-in manifest rather than a synthetic
// one: the thing worth testing is that the real corpus runs, since that is what
// CI executes.
func TestOracleRunsRealCases(t *testing.T) {
	out := filepath.Join(t.TempDir(), "go.jsonl")
	var stdout, stderr strings.Builder
	err := runOracle([]string{
		"--manifest", fixture.Path("cases.jsonl"),
		"--testdata", filepath.Join(fixture.Root(), "testdata"),
		"--out", out,
		"--case", "df.usage_mixed",
		"--case", "ping.ubuntu_no_loss", // no Go adapter yet
	}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("runOracle: %v (stderr: %s)", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("--out was given but %d bytes still went to stdout", stdout.Len())
	}

	recs, order, err := loadRecords(out)
	if err != nil {
		t.Fatalf("loadRecords: %v", err)
	}
	// Manifest order, not flag order: the two oracles must agree on it, and the
	// manifest is the only thing both sides read.
	if want := []string{"df.usage_mixed", "ping.ubuntu_no_loss"}; !equalStrings(order, want) {
		t.Errorf("record order = %v, want %v", order, want)
	}

	ported, _ := recs["df.usage_mixed"].(map[string]any)
	rows, ok := ported["value"].([]any)
	if !ok || len(rows) == 0 {
		t.Fatalf("df.usage_mixed produced no rows: %v", ported)
	}
	if _, isErr := ported["error"]; isErr {
		t.Errorf("a ported function recorded an error: %v", ported)
	}

	unported, _ := recs["ping.ubuntu_no_loss"].(map[string]any)
	if !isUnimplemented(unported) {
		t.Errorf("a function with no adapter recorded %v, want UnknownFunction", unported)
	}
	if got := unportedFn(unported); got != "net.parse_ping_output" {
		t.Errorf("UnknownFunction named %q; the record must carry the manifest name so compare can group by it", got)
	}
}

// A fixture that does not match its manifest hash is corpus corruption, and the
// run stops. Recording it as a result would surface later as a divergence in a
// parser that is fine, sending the reader to the wrong file.
func TestOracleAbortsOnFixtureHashMismatch(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "df"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "Filesystem 1K-blocks Used Available Use% Mounted on\n/dev/sda1 100 50 50 50% /\n"
	if err := os.WriteFile(filepath.Join(dir, "df", "u.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	realHash := hex.EncodeToString(hashOf([]byte(body)))
	manifest := filepath.Join(dir, "cases.jsonl")
	write := func(sha string) {
		line := Case{ID: "df.u", Fn: "disk.parse_df", Fixture: "df/u.txt", SHA256: sha, Input: "text"}
		b, err := json.Marshal(line)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manifest, append(b, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	write(strings.Repeat("0", 64))
	var stdout, stderr strings.Builder
	err := runOracle([]string{"--manifest", manifest, "--testdata", dir}, &stdout, &stderr)
	if err == nil || !strings.Contains(err.Error(), "manifest says") {
		t.Fatalf("runOracle with a bad hash = %v, want a mismatch error", err)
	}
	if stdout.Len() != 0 {
		t.Error("a partial run was written to stdout before the abort")
	}

	// The same manifest with the true hash proves the abort was the hash and not
	// something else about the temp corpus.
	write(realHash)
	if err := runOracle([]string{"--manifest", manifest, "--testdata", dir}, &stdout, &stderr); err != nil {
		t.Fatalf("runOracle with the true hash: %v", err)
	}
}

func TestOracleReportsMissingFixture(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "cases.jsonl")
	body := `{"id":"df.u","fn":"disk.parse_df","fixture":"df/absent.txt","sha256":"x","input":"text"}` + "\n"
	if err := os.WriteFile(manifest, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	err := runOracle([]string{"--manifest", manifest, "--testdata", dir}, &strings.Builder{}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "df.u") {
		t.Fatalf("runOracle = %v, want an error naming the case", err)
	}
}

// The two dispatch tables are hand-written against the same manifest and nothing
// but this test connects them. An adapter registered under a name no case uses is
// dead code that reads as a ported function; one registered with the wrong input
// kind would hand a parser a path where it expects text.
func TestDispatchAgreesWithManifest(t *testing.T) {
	cases, err := loadCases(fixture.Path("cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	inputs := map[string]string{}
	for _, c := range cases {
		if prev, seen := inputs[c.Fn]; seen && prev != c.Input {
			t.Fatalf("manifest gives %s both input=%q and input=%q", c.Fn, prev, c.Input)
		}
		inputs[c.Fn] = c.Input
	}
	for name, ad := range dispatch {
		want, ok := inputs[name]
		if !ok {
			t.Errorf("dispatch registers %q, which no manifest case names", name)
			continue
		}
		if ad.input != want {
			t.Errorf("dispatch[%q].input = %q, manifest says %q", name, ad.input, want)
		}
	}
}

func TestListFunctions(t *testing.T) {
	var stdout, stderr strings.Builder
	if err := runOracle([]string{"--list-functions"}, &stdout, &stderr); err != nil {
		t.Fatal(err)
	}
	got := strings.Fields(stdout.String())
	if len(got) != len(dispatch) {
		t.Fatalf("--list-functions printed %d names, dispatch has %d", len(got), len(dispatch))
	}
	for _, name := range got {
		if _, ok := dispatch[name]; !ok {
			t.Errorf("--list-functions printed %q, which is not in dispatch", name)
		}
	}
}

func TestCaseListFilter(t *testing.T) {
	cases := []Case{{ID: "a"}, {ID: "b"}, {ID: "c"}}

	// Manifest order wins over the order the flags were given in.
	got, err := (&caseList{"c", "a"}).filter(cases)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, len(got))
	for i, c := range got {
		ids[i] = c.ID
	}
	if !equalStrings(ids, []string{"a", "c"}) {
		t.Errorf("filter kept %v, want [a c] in manifest order", ids)
	}

	// A typo in --case must not silently run nothing.
	if _, err := (&caseList{"a", "zz", "yy"}).filter(cases); err == nil ||
		!strings.Contains(err.Error(), "yy, zz") {
		t.Errorf("filter with unknown ids = %v, want an error naming both", err)
	}
}

func TestRedactRewritesOnlyTheInputPath(t *testing.T) {
	in := map[string]any{
		"path":  "/tmp/x/df.txt",
		"other": "/tmp/x/df.txt.bak",
		"rows":  []any{map[string]any{"src": "/tmp/x/df.txt"}, "unrelated"},
		"n":     json.Number("1"),
	}
	got, ok := redact(in, "/tmp/x/df.txt").(map[string]any)
	if !ok {
		t.Fatalf("redact returned %T", got)
	}
	if got["path"] != "<input-path>" {
		t.Errorf("path = %v, want <input-path>", got["path"])
	}
	if got["other"] != "/tmp/x/df.txt.bak" {
		t.Errorf("a path that merely starts with the needle was rewritten: %v", got["other"])
	}
	rows, _ := got["rows"].([]any)
	if nested, _ := rows[0].(map[string]any); nested["src"] != "<input-path>" {
		t.Errorf("nested path not redacted: %v", rows[0])
	}
	if got["n"] != json.Number("1") {
		t.Errorf("redact altered a non-string: %v", got["n"])
	}
}

// No parser takes a path yet, so this registers a stand-in adapter to exercise
// the plumbing before the first real one arrives: the fixture is copied under its
// own basename, the copy is what the adapter is handed, and the path is redacted
// back out of the result so a run is reproducible.
func TestRunCasePathInput(t *testing.T) {
	var handed string
	restore := register(t, "test.path_fn", adapter{
		input: "path",
		run: func(p string, _ map[string]any) (any, error) {
			handed = p
			return map[string]any{"source": p, "size": len(p)}, nil
		},
	})
	defer restore()

	dir := t.TempDir()
	body := "hello\n"
	if err := os.WriteFile(filepath.Join(dir, "cap.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	tmp := t.TempDir()
	c := Case{
		ID: "t.path", Fn: "test.path_fn", Fixture: "cap.txt",
		SHA256: hex.EncodeToString(hashOf([]byte(body))), Input: "path",
	}

	rec, err := runCase(c, dir, tmp)
	if err != nil {
		t.Fatalf("runCase: %v", err)
	}
	if filepath.Dir(handed) != tmp {
		t.Errorf("adapter was handed %q, want a copy under %q", handed, tmp)
	}
	if filepath.Base(handed) != "cap.txt" {
		t.Errorf("the copy is named %q; keeping the fixture basename is what makes a failure traceable", filepath.Base(handed))
	}
	if got, err := os.ReadFile(handed); err != nil || string(got) != body {
		t.Errorf("the copy holds %q (%v), want the fixture bytes", got, err)
	}
	value, _ := rec["value"].(map[string]any)
	if value["source"] != "<input-path>" {
		t.Errorf("the temp path survived into the result: %v", value)
	}
}

// The two dispatch tables are hand-written, so a case whose manifest input kind
// does not match the adapter means they have drifted. Handing a parser a path
// where it expects text would produce a divergence in the parser, which is the
// wrong place to look.
func TestRunCaseRejectsInputKindMismatch(t *testing.T) {
	restore := register(t, "test.text_fn", text(func(string, map[string]any) (any, error) {
		return nil, nil
	}))
	defer restore()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cap.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Case{
		ID: "t.mismatch", Fn: "test.text_fn", Fixture: "cap.txt",
		SHA256: hex.EncodeToString(hashOf([]byte("x"))), Input: "path",
	}
	_, err := runCase(c, dir, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "adapter takes") {
		t.Fatalf("runCase = %v, want a drift error", err)
	}
}

// An adapter whose result will not encode is a bug in the port, not a divergence.
func TestRunCaseRejectsUnencodableResult(t *testing.T) {
	restore := register(t, "test.bad_fn", text(func(string, map[string]any) (any, error) {
		return func() {}, nil
	}))
	defer restore()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "cap.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Case{
		ID: "t.bad", Fn: "test.bad_fn", Fixture: "cap.txt",
		SHA256: hex.EncodeToString(hashOf([]byte("x"))), Input: "text",
	}
	if _, err := runCase(c, dir, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "will not encode") {
		t.Fatalf("runCase = %v, want an encoding error", err)
	}
}

// register adds a stand-in adapter and returns the undo. The manifest-agreement
// test only inspects the entries the real table ships with, so a name that no
// case uses is confined to the test that installs it.
func register(t *testing.T, name string, ad adapter) func() {
	t.Helper()
	if _, taken := dispatch[name]; taken {
		t.Fatalf("%s is a real adapter; pick a name the port will not use", name)
	}
	dispatch[name] = ad
	return func() { delete(dispatch, name) }
}

// A record read out of a malformed oracle file must degrade into something the
// report can print, not panic on a type assertion.
func TestErrorAccessorsTolerateMalformedRecords(t *testing.T) {
	tests := []struct {
		name              string
		rec               string
		code              string
		fn                string
		wantUnimplemented bool
	}{
		{"error is not an object", `{"id":"a","error":"boom"}`, "<malformed error>", "<unknown>", false},
		{"error has no code", `{"id":"a","error":{}}`, "<no code>", "<unknown>", false},
		{"unknown function with no fn", `{"id":"a","error":{"code":"UnknownFunction"}}`, "UnknownFunction", "<unknown>", true},
		{"value record", `{"id":"a","value":1}`, "", "<unknown>", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := decode(t, tt.rec)
			if got := unportedFn(rec); got != tt.fn {
				t.Errorf("unportedFn = %q, want %q", got, tt.fn)
			}
			if got := isUnimplemented(rec); got != tt.wantUnimplemented {
				t.Errorf("isUnimplemented = %v, want %v", got, tt.wantUnimplemented)
			}
			if tt.code != "" {
				m, _ := rec.(map[string]any)
				if got := errCode(m["error"]); got != tt.code {
					t.Errorf("errCode = %q, want %q", got, tt.code)
				}
			}
		})
	}
	// A bare string is not a record at all.
	if unportedFn("nope") != "<unknown>" || isUnimplemented("nope") {
		t.Error("a non-object record was treated as a record")
	}
}

func TestErrorCode(t *testing.T) {
	if got := errorCode(pyError{Code: "ValueError"}); got != "ValueError" {
		t.Errorf("errorCode(pyError) = %q, want ValueError", got)
	}
	// An error the port raises on its own is not a reproduction of Python
	// behaviour and must not be able to compare equal to one.
	if got := errorCode(os.ErrNotExist); got != "GoError" {
		t.Errorf("errorCode(plain error) = %q, want GoError", got)
	}
	if got := (pyError{Code: "KeyError"}).Error(); !strings.Contains(got, "KeyError") {
		t.Errorf("pyError.Error() = %q, want it to name the exception class", got)
	}
}

// Go escapes &, < and > by default. The comparison decodes both files first so it
// would not care, but a human reading a failing case out of the .jsonl would.
func TestMarshalRecordDoesNotEscapeHTML(t *testing.T) {
	line, err := marshalRecord(map[string]any{"id": "a", "value": "x & y <z>"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(line), "x & y <z>") {
		t.Errorf("record was HTML-escaped: %s", line)
	}
	if strings.Contains(string(line), "\n") {
		t.Errorf("record carries its own newline: %q", line)
	}
}

func TestLoadCasesRejectsBadInput(t *testing.T) {
	tests := []struct{ name, body, want string }{
		{"empty", "\n\n", "holds no cases"},
		{"malformed", "{\n", "line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := loadCases(writeTemp(t, "cases.jsonl", tt.body))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("loadCases = %v, want an error mentioning %q", err, tt.want)
			}
		})
	}
	if _, err := loadCases(filepath.Join(t.TempDir(), "absent.jsonl")); err == nil {
		t.Error("loadCases accepted a missing manifest")
	}
}

func writeTemp(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
