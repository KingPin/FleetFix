package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"
	"sort"
	"strings"
)

// Verdicts a case can reach. Kept as a closed set so the report's counts always
// add up to the number of cases compared.
const (
	verdictEqual         = "equal"
	verdictDivergent     = "divergent"
	verdictKnown         = "known-divergence"
	verdictUnimplemented = "unimplemented"
	verdictMissing       = "missing"
)

// Report is what compare writes. Counts first because that is what a reader
// checks; the findings are for whoever has to fix one.
type Report struct {
	Cases          int            `json:"cases"`
	Counts         map[string]int `json:"counts"`
	Normalizations map[string]int `json:"normalizations"`
	// Unported counts the still-missing Go adapters by manifest function name.
	// Kept out of Findings on purpose: while the port is young these outnumber
	// everything else, and a real divergence buried under ninety of them is a
	// divergence nobody reads.
	Unported map[string]int `json:"unported"`
	Findings []Finding      `json:"findings"`
}

// Finding is one case that did not compare equal.
type Finding struct {
	ID      string `json:"id"`
	Verdict string `json:"verdict"`
	Detail  string `json:"detail"`
	Python  string `json:"python"`
	Go      string `json:"go"`
}

func runCompare(argv []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ffdiff compare", flag.ContinueOnError)
	fs.SetOutput(stderr)
	pyPath := fs.String("py", "", "JSON-lines output of py_oracle.py (required)")
	goPath := fs.String("go", "", "JSON-lines output of `ffdiff oracle` (required)")
	out := fs.String("out", "", "write the JSON report here as well as summarising to stdout")
	known := fs.String("known", "", "known_divergences.yaml, if there is one yet")
	requireComplete := fs.Bool("require-complete", false,
		"treat an unported function as a failure; the M2 exit gate runs with this")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if *pyPath == "" || *goPath == "" {
		fs.Usage()
		return fmt.Errorf("both --py and --go are required")
	}

	pyRecords, pyOrder, err := loadRecords(*pyPath)
	if err != nil {
		return err
	}
	goRecords, goOrder, err := loadRecords(*goPath)
	if err != nil {
		return err
	}
	accepted, err := loadKnownDivergences(*known)
	if err != nil {
		return err
	}

	report := compare(pyRecords, pyOrder, goRecords, goOrder, accepted)

	if *out != "" {
		body, err := json.MarshalIndent(report, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(*out, append(body, '\n'), 0o600); err != nil {
			return err
		}
	}
	return summarise(stdout, report, *requireComplete)
}

func compare(py map[string]any, pyOrder []string, goRec map[string]any, goOrder []string, accepted map[string]bool) Report {
	rep := Report{
		Counts:         map[string]int{},
		Normalizations: map[string]int{},
		Unported:       map[string]int{},
	}
	for _, id := range mergedOrder(pyOrder, goOrder) {
		rep.Cases++
		p, inPy := py[id]
		g, inGo := goRec[id]
		switch {
		case !inGo:
			rep.record(id, verdictMissing, "the Go oracle produced no record for this case", p, nil)
		case !inPy:
			rep.record(id, verdictMissing, "the Python oracle produced no record for this case", nil, g)
		case isUnimplemented(g) && !isUnimplemented(p):
			rep.Counts[verdictUnimplemented]++
			rep.Unported[unportedFn(g)]++
		default:
			ok, detail := equalRecords(p, g, rep.Normalizations)
			switch {
			case ok:
				rep.Counts[verdictEqual]++
			case accepted[id]:
				rep.record(id, verdictKnown, detail, p, g)
			default:
				rep.record(id, verdictDivergent, detail, p, g)
			}
		}
	}
	return rep
}

func (r *Report) record(id, verdict, detail string, py, goVal any) {
	r.Counts[verdict]++
	r.Findings = append(r.Findings, Finding{
		ID:      id,
		Verdict: verdict,
		Detail:  detail,
		Python:  brief(py),
		Go:      brief(goVal),
	})
}

// summarise prints the counts and every finding, and decides the exit status.
// Known divergences and, unless --require-complete, unported functions are
// reported without failing: the first is a reviewed decision and the second is
// work that has not happened yet. Everything else fails.
func summarise(w io.Writer, rep Report, requireComplete bool) error {
	order := []string{verdictEqual, verdictDivergent, verdictKnown, verdictUnimplemented, verdictMissing}
	parts := make([]string, 0, len(order))
	for _, v := range order {
		parts = append(parts, fmt.Sprintf("%s=%d", v, rep.Counts[v]))
	}
	fmt.Fprintf(w, "%d cases: %s\n", rep.Cases, strings.Join(parts, " "))

	for _, name := range sortedCounts(rep.Normalizations) {
		fmt.Fprintf(w, "normalized: %s on %d value(s)\n", name, rep.Normalizations[name])
	}
	for _, fn := range sortedCounts(rep.Unported) {
		fmt.Fprintf(w, "unported: %s (%d case(s))\n", fn, rep.Unported[fn])
	}
	for _, f := range rep.Findings {
		fmt.Fprintf(w, "\n%s [%s]\n  %s\n  py: %s\n  go: %s\n", f.ID, f.Verdict, f.Detail, f.Python, f.Go)
	}

	bad := rep.Counts[verdictDivergent] + rep.Counts[verdictMissing]
	if requireComplete {
		bad += rep.Counts[verdictUnimplemented]
	}
	if bad > 0 {
		return fmt.Errorf("%d case(s) need attention", bad)
	}
	return nil
}

// sortedCounts returns the normalization names in a stable order.
func sortedCounts(m map[string]int) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// equalRecords compares two whole records. An error record compares on its code
// only; a value record compares on the decoded value.
func equalRecords(py, goVal any, norms map[string]int) (bool, string) {
	pm, pOK := py.(map[string]any)
	gm, gOK := goVal.(map[string]any)
	if !pOK || !gOK {
		return false, "one side is not a JSON object"
	}
	pErr, pHasErr := pm["error"]
	gErr, gHasErr := gm["error"]
	switch {
	case pHasErr && gHasErr:
		if pc, gc := errCode(pErr), errCode(gErr); pc != gc {
			return false, fmt.Sprintf("error code %s vs %s", pc, gc)
		}
		return true, ""
	case pHasErr:
		return false, fmt.Sprintf("Python raised %s, Go returned a value", errCode(pErr))
	case gHasErr:
		return false, fmt.Sprintf("Go returned %s, Python produced a value", errCode(gErr))
	}
	return equalValues(pm["value"], gm["value"], "value", norms)
}

// equalValues is the comparison the whole harness rests on: decoded values, with
// the four cosmetic rules the design calls for and nothing else. Anything it
// cannot explain away is a real difference.
func equalValues(py, goVal any, path string, norms map[string]int) (bool, string) {
	py, goVal = normalizeEmpty(py, goVal, norms)

	switch p := py.(type) {
	case nil:
		if goVal == nil {
			return true, ""
		}
		return false, fmt.Sprintf("%s: null vs %s", path, brief(goVal))

	case json.Number:
		g, ok := goVal.(json.Number)
		if !ok {
			return false, fmt.Sprintf("%s: %s vs %s", path, brief(py), brief(goVal))
		}
		return equalNumbers(p, g, path)

	case string:
		g, ok := goVal.(string)
		if !ok || p != g {
			return false, fmt.Sprintf("%s: %s vs %s", path, brief(py), brief(goVal))
		}
		return true, ""

	case bool:
		g, ok := goVal.(bool)
		if !ok || p != g {
			return false, fmt.Sprintf("%s: %s vs %s", path, brief(py), brief(goVal))
		}
		return true, ""

	case []any:
		g, ok := goVal.([]any)
		if !ok {
			return false, fmt.Sprintf("%s: list vs %s", path, brief(goVal))
		}
		if len(p) != len(g) {
			return false, fmt.Sprintf("%s: %d element(s) vs %d", path, len(p), len(g))
		}
		for i := range p {
			if ok, detail := equalValues(p[i], g[i], fmt.Sprintf("%s[%d]", path, i), norms); !ok {
				return false, detail
			}
		}
		return true, ""

	case map[string]any:
		g, ok := goVal.(map[string]any)
		if !ok {
			return false, fmt.Sprintf("%s: object vs %s", path, brief(goVal))
		}
		// Walk the union of the keys rather than each side's keys in turn. Dropping
		// the null-valued keys up front would look equivalent and is not: it leaves
		// the other side's empty collection with no partner, so {"a": null} against
		// {"a": []} would be reported as a key only Go has instead of reaching the
		// null-vs-empty rule below.
		for _, k := range unionKeys(p, g) {
			pv, pHas := p[k]
			gv, gHas := g[k]
			switch {
			case pHas && gHas:
				if ok, detail := equalValues(pv, gv, path+"."+k, norms); !ok {
					return false, detail
				}
			case isEmptyish(pv) && isEmptyish(gv):
				// One side omits the key and the other holds null or an empty
				// collection. A missing key reads as nil here, so this is the
				// absent-vs-null rule and, by the same reasoning, absent-vs-empty.
				norms["absent-vs-null-key"]++
			case pHas:
				return false, fmt.Sprintf("%s.%s: only Python has this key", path, k)
			default:
				return false, fmt.Sprintf("%s.%s: only Go has this key", path, k)
			}
		}
		return true, ""

	default:
		return false, fmt.Sprintf("%s: unhandled JSON type %T", path, py)
	}
}

// normalizeEmpty makes null and an empty collection the same value. The real fix
// is at the source -- a wire struct must not hold a nil slice -- and this is the
// safety net, counted in the report so it can never quietly absorb a difference
// that matters.
func normalizeEmpty(py, goVal any, norms map[string]int) (any, any) {
	if isEmptyish(py) && isEmptyish(goVal) && (py == nil) != (goVal == nil) {
		norms["null-vs-empty-collection"]++
		return nil, nil
	}
	return py, goVal
}

func isEmptyish(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	default:
		return false
	}
}

// unionKeys returns every key either object holds, in a stable order.
func unionKeys(a, b map[string]any) []string {
	keys := make([]string, 0, len(a)+len(b))
	for k := range a {
		keys = append(keys, k)
	}
	for k := range b {
		if _, both := a[k]; !both {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	return keys
}

// equalNumbers unifies 50 and 50.0 without ever pushing a large integer through
// a float. Python's ints are arbitrary precision and JSON has no integer type,
// so a 64-bit-plus value that round-tripped through float64 would compare equal
// to a neighbour it is not equal to.
func equalNumbers(py, goVal json.Number, path string) (bool, string) {
	if py.String() == goVal.String() {
		return true, ""
	}
	pi, pOK := new(big.Int).SetString(py.String(), 10)
	gi, gOK := new(big.Int).SetString(goVal.String(), 10)
	if pOK && gOK {
		if pi.Cmp(gi) == 0 {
			return true, ""
		}
		return false, fmt.Sprintf("%s: %s vs %s", path, py, goVal)
	}
	pf, pErr := py.Float64()
	gf, gErr := goVal.Float64()
	if pErr != nil || gErr != nil {
		return false, fmt.Sprintf("%s: %s vs %s (unparseable as a number)", path, py, goVal)
	}
	if closeEnough(pf, gf) {
		return true, ""
	}
	return false, fmt.Sprintf("%s: %s vs %s", path, py, goVal)
}

// closeEnough compares floats within a relative epsilon. Two implementations
// summing the same numbers in a different order land a few ULPs apart, which is
// not a divergence anyone can act on.
func closeEnough(a, b float64) bool {
	if a == b {
		return true
	}
	if math.IsNaN(a) || math.IsNaN(b) || math.IsInf(a, 0) || math.IsInf(b, 0) {
		return false
	}
	scale := math.Max(math.Abs(a), math.Abs(b))
	return math.Abs(a-b) <= 1e-9*scale
}

func errCode(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "<malformed error>"
	}
	code, _ := m["code"].(string)
	if code == "" {
		return "<no code>"
	}
	return code
}

// unportedFn reads the manifest function name back out of an UnknownFunction
// record, so the report names the work rather than the ninety cases waiting on it.
func unportedFn(rec any) string {
	m, ok := rec.(map[string]any)
	if !ok {
		return "<unknown>"
	}
	e, ok := m["error"].(map[string]any)
	if !ok {
		return "<unknown>"
	}
	fn, _ := e["fn"].(string)
	if fn == "" {
		return "<unknown>"
	}
	return fn
}

func isUnimplemented(rec any) bool {
	m, ok := rec.(map[string]any)
	if !ok {
		return false
	}
	e, ok := m["error"]
	return ok && errCode(e) == "UnknownFunction"
}

// loadRecords reads a JSON-lines oracle output, keeping the manifest order the
// file was written in so the report reads in the same order as the corpus.
func loadRecords(path string) (map[string]any, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	records := map[string]any{}
	var order []string
	for i, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		v, err := decodeValue([]byte(line))
		if err != nil {
			return nil, nil, fmt.Errorf("%s line %d: %w", path, i+1, err)
		}
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s line %d: not a JSON object", path, i+1)
		}
		id, _ := m["id"].(string)
		if id == "" {
			return nil, nil, fmt.Errorf("%s line %d: record has no id", path, i+1)
		}
		if _, seen := records[id]; seen {
			return nil, nil, fmt.Errorf("%s line %d: duplicate id %q", path, i+1, id)
		}
		records[id] = m
		order = append(order, id)
	}
	if len(records) == 0 {
		return nil, nil, fmt.Errorf("%s holds no records", path)
	}
	return records, order, nil
}

// mergedOrder is the Python order, then anything only the Go side produced.
func mergedOrder(pyOrder, goOrder []string) []string {
	seen := make(map[string]bool, len(pyOrder))
	out := make([]string, 0, len(pyOrder)+len(goOrder))
	for _, id := range pyOrder {
		seen[id] = true
		out = append(out, id)
	}
	for _, id := range goOrder {
		if !seen[id] {
			out = append(out, id)
		}
	}
	return out
}

// decodeValue decodes JSON with numbers left as their literal text, so an
// integer wider than a float64's mantissa survives to be compared exactly.
func decodeValue(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// brief renders a value for the report: enough to recognise, short enough that a
// hundred findings stay readable.
func brief(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("<unrenderable %T>", v)
	}
	const max = 240
	if len(b) > max {
		return string(b[:max]) + "…"
	}
	return string(b)
}
