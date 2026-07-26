package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// decode is the shorthand these tests use to build a value exactly as the
// comparison will see it: through the same UseNumber decoder, so a literal in a
// case below behaves like a literal in an oracle file.
func decode(t *testing.T, s string) any {
	t.Helper()
	v, err := decodeValue([]byte(s))
	if err != nil {
		t.Fatalf("decodeValue(%s): %v", s, err)
	}
	return v
}

func TestEqualValues(t *testing.T) {
	tests := []struct {
		name  string
		py    string
		goVal string
		equal bool
	}{
		{"identical objects", `{"a":1,"b":"x"}`, `{"a":1,"b":"x"}`, true},
		{"nested", `{"a":[{"b":[1,2]}]}`, `{"a":[{"b":[1,2]}]}`, true},

		// Numeric unification: JSON has one number type and the two encoders
		// spell the same value differently.
		{"int and float spelling", `{"a":50}`, `{"a":50.0}`, true},
		{"exponent spelling", `{"a":1000}`, `{"a":1e3}`, true},
		{"float within epsilon", `{"a":0.30000000000000004}`, `{"a":0.3}`, true},
		{"different numbers", `{"a":50}`, `{"a":51}`, false},

		// Integers wider than a float64 mantissa must compare exactly. Pushed
		// through a float these two are the same value.
		{"large ints that differ", `9007199254740993`, `9007199254740992`, false},
		{"large ints that match", `9007199254740993`, `9007199254740993`, true},

		// Cosmetic rules.
		{"null and empty list", `{"a":null}`, `{"a":[]}`, true},
		{"null and empty object", `{"a":null}`, `{"a":{}}`, true},
		{"null and absent key", `{"a":1,"b":null}`, `{"a":1}`, true},
		{"absent key and null", `{"a":1}`, `{"a":1,"b":null}`, true},
		{"absent key and empty collection", `{"a":1}`, `{"a":1,"b":[]}`, true},
		{"empty list and populated list", `{"a":[]}`, `{"a":[1]}`, false},

		{"key only in python", `{"a":1,"b":2}`, `{"a":1}`, false},
		{"key only in go", `{"a":1}`, `{"a":1,"b":2}`, false},
		{"list length", `[1,2]`, `[1,2,3]`, false},
		{"string difference", `"a"`, `"b"`, false},
		{"bool difference", `true`, `false`, false},
		{"type difference", `"1"`, `1`, false},
		{"null versus a value", `null`, `1`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			norms := map[string]int{}
			ok, detail := equalValues(decode(t, tt.py), decode(t, tt.goVal), "value", norms)
			if ok != tt.equal {
				t.Fatalf("equalValues(%s, %s) = %v (%s), want %v", tt.py, tt.goVal, ok, detail, tt.equal)
			}
			if !ok && detail == "" {
				t.Error("a difference was reported with no detail; the report would say nothing useful")
			}
		})
	}
}

// The cosmetic rules must be visible in the report. A normalization that fires
// silently is indistinguishable from a comparison that is not happening.
func TestNormalizationsAreCounted(t *testing.T) {
	norms := map[string]int{}
	if ok, detail := equalValues(decode(t, `{"a":null,"b":null}`), decode(t, `{"a":[]}`), "value", norms); !ok {
		t.Fatalf("expected equal, got %s", detail)
	}
	if norms["null-vs-empty-collection"] != 1 {
		t.Errorf("null-vs-empty-collection counted %d times, want 1", norms["null-vs-empty-collection"])
	}
	if norms["absent-vs-null-key"] != 1 {
		t.Errorf("absent-vs-null-key counted %d times, want 1", norms["absent-vs-null-key"])
	}
}

func TestEqualRecords(t *testing.T) {
	tests := []struct {
		name  string
		py    string
		goVal string
		equal bool
	}{
		{"same value", `{"id":"a","value":[1]}`, `{"id":"a","value":[1]}`, true},
		{"same error code", `{"id":"a","error":{"code":"ValueError"}}`, `{"id":"a","error":{"code":"ValueError"}}`, true},
		{"different error code", `{"id":"a","error":{"code":"ValueError"}}`, `{"id":"a","error":{"code":"KeyError"}}`, false},
		{"python raised, go did not", `{"id":"a","error":{"code":"ValueError"}}`, `{"id":"a","value":[]}`, false},
		{"go errored, python did not", `{"id":"a","value":[]}`, `{"id":"a","error":{"code":"GoError"}}`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ok, detail := equalRecords(decode(t, tt.py), decode(t, tt.goVal), map[string]int{})
			if ok != tt.equal {
				t.Fatalf("equalRecords = %v (%s), want %v", ok, detail, tt.equal)
			}
		})
	}
}

// An error message is deliberately not compared: Python's prose cannot be
// reproduced in Go, so comparing it would generate divergences nobody can fix.
func TestErrorMessagesAreNotCompared(t *testing.T) {
	py := decode(t, `{"id":"a","error":{"code":"ValueError","message":"invalid literal for int()"}}`)
	goVal := decode(t, `{"id":"a","error":{"code":"ValueError"}}`)
	if ok, detail := equalRecords(py, goVal, map[string]int{}); !ok {
		t.Errorf("records with the same code compared unequal: %s", detail)
	}
}

func TestCompareClassifies(t *testing.T) {
	py, pyOrder := records(
		t,
		`{"id":"same","value":1}`,
		`{"id":"diff","value":1}`,
		`{"id":"todo","value":1}`,
		`{"id":"accepted","value":1}`,
		`{"id":"gone","value":1}`,
	)
	goRec, goOrder := records(
		t,
		`{"id":"same","value":1}`,
		`{"id":"diff","value":2}`,
		`{"id":"todo","error":{"code":"UnknownFunction","fn":"net.parse_ping_output"}}`,
		`{"id":"accepted","value":3}`,
		`{"id":"extra","value":1}`,
	)

	rep := compare(py, pyOrder, goRec, goOrder, map[string]bool{"accepted": true})

	if rep.Cases != 6 {
		t.Errorf("Cases = %d, want 6", rep.Cases)
	}
	want := map[string]int{
		verdictEqual:         1,
		verdictDivergent:     1,
		verdictKnown:         1,
		verdictUnimplemented: 1,
		verdictMissing:       2,
	}
	for verdict, n := range want {
		if rep.Counts[verdict] != n {
			t.Errorf("Counts[%s] = %d, want %d", verdict, rep.Counts[verdict], n)
		}
	}
	if rep.Unported["net.parse_ping_output"] != 1 {
		t.Errorf("Unported = %v, want net.parse_ping_output counted once", rep.Unported)
	}
	// Unported cases stay out of Findings so a real divergence is not buried.
	for _, f := range rep.Findings {
		if f.Verdict == verdictUnimplemented {
			t.Errorf("unimplemented case %s reached Findings", f.ID)
		}
	}
}

// The verdict a case reaches decides whether the harness fails, so the mapping
// from counts to exit status gets its own test rather than being implied by the
// classification one.
func TestSummariseExitStatus(t *testing.T) {
	tests := []struct {
		name            string
		counts          map[string]int
		requireComplete bool
		wantErr         bool
	}{
		{"all equal", map[string]int{verdictEqual: 3}, false, false},
		{"known divergence does not fail", map[string]int{verdictKnown: 1}, false, false},
		{"unimplemented does not fail by default", map[string]int{verdictUnimplemented: 91}, false, false},
		{"unimplemented fails the exit gate", map[string]int{verdictUnimplemented: 1}, true, true},
		{"divergence fails", map[string]int{verdictDivergent: 1}, false, true},
		{"missing record fails", map[string]int{verdictMissing: 1}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rep := Report{Counts: tt.counts, Normalizations: map[string]int{}, Unported: map[string]int{}}
			err := summarise(&strings.Builder{}, rep, tt.requireComplete)
			if (err != nil) != tt.wantErr {
				t.Errorf("summarise error = %v, want error: %v", err, tt.wantErr)
			}
		})
	}
}

func TestSummarisePrintsEveryFinding(t *testing.T) {
	rep := Report{
		Cases:          1,
		Counts:         map[string]int{verdictDivergent: 1},
		Normalizations: map[string]int{"absent-vs-null-key": 2},
		Unported:       map[string]int{"net.parse_ping_output": 5},
		Findings: []Finding{
			{ID: "df.usage_mixed", Verdict: verdictDivergent, Detail: "value[0].used_pct: 50 vs 51", Python: "50", Go: "51"},
		},
	}
	var out strings.Builder
	if err := summarise(&out, rep, false); err == nil {
		t.Fatal("summarise returned nil for a divergent report")
	}
	for _, want := range []string{
		"df.usage_mixed", "value[0].used_pct: 50 vs 51",
		"absent-vs-null-key", "net.parse_ping_output",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("summary does not mention %q:\n%s", want, out.String())
		}
	}
}

func TestBriefTruncatesLongValues(t *testing.T) {
	long := make([]int, 500)
	b, err := json.Marshal(long)
	if err != nil {
		t.Fatal(err)
	}
	got := brief(decode(t, string(b)))
	if !strings.HasSuffix(got, "…") {
		t.Errorf("brief did not truncate a %d-byte value: %s", len(b), got)
	}
}

func TestLoadRecordsRejectsBadInput(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"empty file", "\n\n", "holds no records"},
		{"not an object", "[1,2]\n", "not a JSON object"},
		{"no id", `{"value":1}` + "\n", "has no id"},
		{"duplicate id", `{"id":"a","value":1}` + "\n" + `{"id":"a","value":2}` + "\n", "duplicate id"},
		{"malformed json", `{"id":`, "line 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeTemp(t, "records.jsonl", tt.body)
			_, _, err := loadRecords(path)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("loadRecords error = %v, want one mentioning %q", err, tt.want)
			}
		})
	}
}

// TestRunCompareEndToEnd drives the mode the way CI does: two files in, a report
// file and a summary out, an exit status that reflects the worst verdict.
func TestRunCompareEndToEnd(t *testing.T) {
	dir := t.TempDir()
	py := filepath.Join(dir, "py.jsonl")
	goFile := filepath.Join(dir, "go.jsonl")
	report := filepath.Join(dir, "report.json")
	writeFile(t, py, `{"id":"a","value":{"pct":50}}`+"\n"+`{"id":"b","value":1}`+"\n")
	writeFile(t, goFile, `{"id":"a","value":{"pct":50.0}}`+"\n"+`{"id":"b","value":2}`+"\n")

	var stdout, stderr strings.Builder
	err := runCompare([]string{"--py", py, "--go", goFile, "--out", report}, &stdout, &stderr)
	if err == nil {
		t.Fatal("runCompare returned nil with a divergent case")
	}
	if !strings.Contains(stdout.String(), "equal=1 divergent=1") {
		t.Errorf("summary does not carry the counts:\n%s", stdout.String())
	}

	var got Report
	body, readErr := os.ReadFile(report)
	if readErr != nil {
		t.Fatalf("no report written: %v", readErr)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("report is not valid JSON: %v", err)
	}
	if got.Cases != 2 || got.Counts[verdictDivergent] != 1 || len(got.Findings) != 1 {
		t.Errorf("report = %+v, want 2 cases with one divergent finding", got)
	}

	// The same run with the divergence accepted exits clean.
	known := filepath.Join(dir, "known.yaml")
	writeFile(t, known, "divergences:\n  - id: b\n    reason: pinned by the port\n    issue: \"#9\"\n")
	stdout.Reset()
	if err := runCompare([]string{"--py", py, "--go", goFile, "--known", known}, &stdout, &stderr); err != nil {
		t.Fatalf("runCompare with the divergence accepted: %v", err)
	}
	if !strings.Contains(stdout.String(), "known-divergence=1") {
		t.Errorf("an accepted divergence is not reported as one:\n%s", stdout.String())
	}
}

func TestRunCompareRequiresBothInputs(t *testing.T) {
	path := writeTemp(t, "x.jsonl", `{"id":"a","value":1}`+"\n")
	for _, argv := range [][]string{{}, {"--py", path}, {"--go", path}} {
		err := runCompare(argv, &strings.Builder{}, &strings.Builder{})
		if err == nil || !strings.Contains(err.Error(), "required") {
			t.Errorf("runCompare(%v) = %v, want a missing-flag error", argv, err)
		}
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// records builds the (map, order) pair loadRecords would produce, without a file.
func records(t *testing.T, lines ...string) (map[string]any, []string) {
	t.Helper()
	out := map[string]any{}
	order := make([]string, 0, len(lines))
	for _, line := range lines {
		m, ok := decode(t, line).(map[string]any)
		if !ok {
			t.Fatalf("test record is not an object: %s", line)
		}
		id, _ := m["id"].(string)
		out[id] = m
		order = append(order, id)
	}
	return out, order
}
