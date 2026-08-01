package main

import (
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/google/go-cmp/cmp"
	"gopkg.in/yaml.v3"
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
		"--case", "ping.ubuntu_no_loss",
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

	rows, _ := recs["df.usage_mixed"].(map[string]any)
	if _, ok := rows["value"].([]any); !ok {
		t.Errorf("df.usage_mixed produced no rows: %v", rows)
	}
	// The second case takes a manifest argument, so it also proves the args are
	// reaching the adapter rather than every case being called bare.
	ping, _ := recs["ping.ubuntu_no_loss"].(map[string]any)
	summary, _ := ping["value"].(map[string]any)
	if summary["target"] != "8.8.8.8" {
		t.Errorf(`ping.ubuntu_no_loss recorded %v, want its manifest target "8.8.8.8"`, ping)
	}
	for _, id := range order {
		if rec, _ := recs[id].(map[string]any); rec["error"] != nil {
			t.Errorf("a ported function recorded an error: %v", rec)
		}
	}
}

// TestOracleRunsEveryCaseInTheCorpus is the Go half of a pair: py_oracle has
// test_run_produces_one_record_per_case_and_only_documented_errors, and until now
// nothing said the same thing about this side. The narrow test above proves the
// plumbing on two cases; this one proves every adapter can run the cases the
// manifest points at it.
//
// It matters because an adapter is only reached through the dispatch table, so a
// closure that reads the wrong argument name, hands a parser the wrong field, or
// panics on a fixture it has never seen is invisible to the unit tests of the
// parser underneath it. In CI that surfaces as an error record, which `compare`
// counts as a divergence -- a failure attributed to the port rather than to the
// harness. Here it names the case.
//
// An error is allowed only where a divergence entry argues for one. Those entries
// are written about the Python side (v1 raises, Go answers), so today the set is
// empty on this side; keying on the same file rather than on a literal list means
// a future Go-raises divergence needs an argued entry rather than an edit here.
func TestOracleRunsEveryCaseInTheCorpus(t *testing.T) {
	out := filepath.Join(t.TempDir(), "go.jsonl")
	var stdout, stderr strings.Builder
	if err := runOracle([]string{
		"--manifest", fixture.Path("cases.jsonl"),
		"--testdata", filepath.Join(fixture.Root(), "testdata"),
		"--out", out,
	}, &stdout, &stderr); err != nil {
		t.Fatalf("runOracle: %v (stderr: %s)", err, stderr.String())
	}

	cases, err := loadCases(fixture.Path("cases.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	recs, order, err := loadRecords(out)
	if err != nil {
		t.Fatalf("loadRecords: %v", err)
	}
	want := make([]string, len(cases))
	for i, c := range cases {
		want[i] = c.ID
	}
	if !equalStrings(order, want) {
		t.Fatalf("the oracle wrote %d records for %d cases, or reordered them", len(order), len(want))
	}

	known, err := knownDivergenceIDs()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range order {
		rec, _ := recs[id].(map[string]any)
		if rec["error"] == nil {
			continue
		}
		// Unported is a different claim from broken, and the manifest-agreement
		// test is what keeps it honest, so say which one this is.
		switch {
		case isUnimplemented(rec):
			t.Errorf("%s: no adapter for %s", id, unportedFn(rec))
		case !known[id]:
			t.Errorf("%s: the adapter could not run its own case: %v", id, rec["error"])
		}
	}
}

// knownDivergenceIDs reads the ids out of the file `compare` reads, rather than a
// copy: a list maintained here would drift, and drifting towards permissive is
// how an error stops being noticed.
func knownDivergenceIDs() (map[string]bool, error) {
	body, err := os.ReadFile(filepath.Join(fixture.Root(), "differential", "known_divergences.yaml"))
	if err != nil {
		return nil, err
	}
	var doc struct {
		Divergences []struct {
			ID string `yaml:"id"`
		} `yaml:"divergences"`
	}
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	ids := make(map[string]bool, len(doc.Divergences))
	for _, d := range doc.Divergences {
		ids[d.ID] = true
	}
	return ids, nil
}

// A case naming a function no adapter implements is the port's normal
// intermediate state, and the record has to say so in a form `compare` can group
// by. Built on a synthetic manifest rather than a real unported case: which real
// functions are unported changes with every parser that lands, and this is about
// the record's shape, not about today's gap.
func TestOracleRecordsUnknownFunction(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "df"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "irrelevant: no adapter will ever read it\n"
	if err := os.WriteFile(filepath.Join(dir, "df", "u.txt"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// A real fixture and its true hash: runCase verifies the corpus before it
	// consults dispatch, so a placeholder hash would abort ahead of the path
	// under test.
	line, err := json.Marshal(Case{
		ID: "never.case", Fn: "never.ported", Fixture: "df/u.txt",
		SHA256: hex.EncodeToString(hashOf([]byte(body))), Input: "text",
	})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "cases.jsonl")
	if err := os.WriteFile(manifest, append(line, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}

	out := filepath.Join(dir, "go.jsonl")
	var stdout, stderr strings.Builder
	if err := runOracle([]string{
		"--manifest", manifest, "--testdata", dir, "--out", out,
	}, &stdout, &stderr); err != nil {
		t.Fatalf("runOracle: %v (stderr: %s)", err, stderr.String())
	}

	recs, _, err := loadRecords(out)
	if err != nil {
		t.Fatalf("loadRecords: %v", err)
	}
	rec, _ := recs["never.case"].(map[string]any)
	if !isUnimplemented(rec) {
		t.Errorf("a function with no adapter recorded %v, want UnknownFunction", rec)
	}
	if got := unportedFn(rec); got != "never.ported" {
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

// An adapter's arguments come from the manifest, and the manifest is shared with
// py_oracle. A missing or mistyped one is the two tables disagreeing about a
// case's signature, which has to surface as an error on the case rather than as
// a zero value that reads like a target nobody set or a hop limit of nought.
func TestAdapterArgumentsAreCheckedNotAssumed(t *testing.T) {
	strTests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"present", map[string]any{"target": "8.8.8.8"}, ""},
		{"absent", map[string]any{}, `no "target" argument`},
		{"wrong type", map[string]any{"target": 1.0}, `is float64, want a string`},
	}
	for _, tt := range strTests {
		t.Run("target/"+tt.name, func(t *testing.T) {
			_, err := strArg(tt.args, "target")
			assertErrContains(t, err, tt.want)
		})
	}

	intTests := []struct {
		name string
		args map[string]any
		want string
	}{
		// JSON has one number type, so a whole number decodes as a float64.
		{"present", map[string]any{"max_hops": 15.0}, ""},
		{"absent", map[string]any{}, `no "max_hops" argument`},
		{"wrong type", map[string]any{"max_hops": "15"}, `is string, want a number`},
		{"fractional", map[string]any{"max_hops": 1.5}, `want a whole number`},
	}
	for _, tt := range intTests {
		t.Run("max_hops/"+tt.name, func(t *testing.T) {
			_, err := intArg(tt.args, "max_hops")
			assertErrContains(t, err, tt.want)
		})
	}

	// And the adapters themselves propagate it rather than parsing with a hole.
	for _, fn := range []string{"net.parse_ping_output", "net.parse_traceroute_output"} {
		if _, err := dispatch[fn].run("", map[string]any{}); err == nil {
			t.Errorf("dispatch[%q] ran with no arguments at all", fn)
		}
	}
}

func assertErrContains(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case want == "" && err != nil:
		t.Fatalf("got %v, want no error", err)
	case want != "" && (err == nil || !strings.Contains(err.Error(), want)):
		t.Fatalf("got %v, want an error mentioning %q", err, want)
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

// The pairing to py_oracle's plain(): a non-finite float has to survive the trip
// as something strict JSON can hold, because Go's decoder cannot read the
// Infinity/NaN words Python's json.dumps writes and probes.yml is allowed to say
// `.inf`. Each sign gets its own tag so a +inf turning into a -inf still fails.
func TestCanonicalTagsNonFiniteFloats(t *testing.T) {
	got, err := canonical(map[string]any{
		"pos":    math.Inf(1),
		"neg":    math.Inf(-1),
		"nan":    math.NaN(),
		"finite": 1.5,
		"nested": []any{math.Inf(1), "x"},
	})
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	want := map[string]any{
		"pos":    "<+inf>",
		"neg":    "<-inf>",
		"nan":    "<nan>",
		"finite": json.Number("1.5"),
		"nested": []any{"<+inf>", "x"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("canonical mismatch (-want +got):\n%s", diff)
	}
}

// A stand-in adapter rather than a real one, so a failure here is the plumbing
// and not a parser built on it: the fixture is copied under its own basename, the
// copy is what the adapter is handed, and the path is redacted back out of the
// result so a run is reproducible.
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

// The tree kind materialises a JSON directory description into a real directory
// rather than an in-memory filesystem, so both oracles hand their implementation
// the same answers for the shapes a synthesised filesystem gets subtly wrong: a
// file where a directory was expected, and a directory with nothing in it.
func TestRunCaseTreeInput(t *testing.T) {
	restore := register(t, "test.tree_fn", treeFS(func(fsys fs.FS, _ map[string]any) (any, error) {
		var seen []string
		err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			kind := "file"
			if d.IsDir() {
				kind = "dir"
			}
			seen = append(seen, kind+" "+p)
			return nil
		})
		return seen, err
	}))
	defer restore()

	dir := t.TempDir()
	// An empty object is a directory with nothing in it -- a driver that registered
	// and then failed -- and a string at the top level is a plain file where a
	// caller walking the tree expects a directory.
	body := `{"zone0": {"temp": "42000\n"}, "zone1": {}, "notadir": "plain"}`
	if err := os.WriteFile(filepath.Join(dir, "tree.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Case{
		ID: "t.tree", Fn: "test.tree_fn", Fixture: "tree.json",
		SHA256: hex.EncodeToString(hashOf([]byte(body))), Input: "tree",
	}

	rec, err := runCase(c, dir, t.TempDir())
	if err != nil {
		t.Fatalf("runCase: %v", err)
	}
	want := []string{"dir .", "file notadir", "dir zone0", "file zone0/temp", "dir zone1"}
	value, _ := rec["value"].([]any)
	got := make([]string, len(value))
	for i, elem := range value {
		got[i], _ = elem.(string)
	}
	if !equalStrings(got, want) {
		t.Errorf("adapter walked %v, want %v", got, want)
	}
}

// Two cases may share one tree fixture -- read_zones and hottest both read the
// thermal capture -- and must not be able to see each other's writes, even though
// nothing materialises a tree for writing today.
func TestRunCaseTreeGivesEachCaseItsOwnDirectory(t *testing.T) {
	var roots []string
	restore := register(t, "test.tree_root_fn", adapter{
		input: "tree",
		run: func(root string, _ map[string]any) (any, error) {
			roots = append(roots, root)
			return nil, nil
		},
	})
	defer restore()

	dir := t.TempDir()
	body := `{"a": "1"}`
	if err := os.WriteFile(filepath.Join(dir, "tree.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := hex.EncodeToString(hashOf([]byte(body)))
	tmp := t.TempDir()
	for _, id := range []string{"t.tree", "t.tree#variant"} {
		c := Case{ID: id, Fn: "test.tree_root_fn", Fixture: "tree.json", SHA256: sum, Input: "tree"}
		if _, err := runCase(c, dir, tmp); err != nil {
			t.Fatalf("runCase %s: %v", id, err)
		}
	}
	if roots[0] == roots[1] {
		t.Errorf("both cases were handed %q", roots[0])
	}
	// The id becomes a single directory name, so its separators cannot survive.
	for _, root := range roots {
		if base := filepath.Base(root); strings.ContainsAny(base, "#/") {
			t.Errorf("root %q keeps a character that is not a directory name", base)
		}
		if filepath.Dir(root) != tmp {
			t.Errorf("root %q is not under the run's temp directory %q", root, tmp)
		}
	}
}

// A tree fixture that will not decode is corpus corruption, not a result: recording
// it as an error code would read as a divergence in the reader.
func TestRunCaseRejectsAMalformedTree(t *testing.T) {
	restore := register(t, "test.tree_bad_fn", treeFS(func(fs.FS, map[string]any) (any, error) {
		return nil, nil
	}))
	defer restore()

	dir := t.TempDir()
	body := `["not an object"]`
	if err := os.WriteFile(filepath.Join(dir, "tree.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c := Case{
		ID: "t.badtree", Fn: "test.tree_bad_fn", Fixture: "tree.json",
		SHA256: hex.EncodeToString(hashOf([]byte(body))), Input: "tree",
	}
	_, err := runCase(c, dir, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "tree.json") {
		t.Fatalf("runCase = %v, want an error naming the fixture", err)
	}
}

// A number or a list inside the tree has no filesystem meaning, and guessing one
// would let a mistyped fixture materialise as something nobody wrote.
func TestWriteTreeRejectsAValueThatIsNeither(t *testing.T) {
	err := writeTree(map[string]any{"millidegrees": 42000.0}, filepath.Join(t.TempDir(), "root"))
	if err == nil || !strings.Contains(err.Error(), "millidegrees") {
		t.Fatalf("writeTree = %v, want an error naming the entry", err)
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
