package config

// The PyYAML compatibility suite.
//
// testdata/pyyaml/pin.json holds what v1's config._read_yaml_mapping actually
// returned for each of the documents in testdata/pyyaml/corpus.json, measured by
// running it -- see tools/oracle/pin_pyyaml.py. This test replays the corpus
// through parseYAMLMapping and requires the same answer.
//
// It is not part of the differential harness. The manifest only holds the eight
// real config files an operator would write, which says nothing about YAML dialect
// questions, and the harness dies with the Python at M7 while this pin does not.

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

// knownDivergences names every corpus case where this package deliberately answers
// differently from PyYAML, with the reason. Nine of 137.
//
// The test fails both ways: an unlisted divergence is a bug, and a listed case that
// starts matching is a stale entry to delete. Neither is allowed to drift quietly,
// which is the M2 exit gate -- 100% equivalence or every divergence justified.
var knownDivergences = map[string]string{
	// Python has date, naive datetime and aware datetime; Go has time.Time. Dates
	// and naive stamps land at UTC. Unobservable through the JSON contract, since
	// json.dumps refuses all three types outright.
	"date":                   "datetime.date collapses to time.Time at midnight UTC",
	"explicit timestamp tag": "datetime.date collapses to time.Time at midnight UTC",
	"timestamp spaced":       "a naive datetime collapses to time.Time in UTC",

	// A Go set is keyed by the coerced key string, so non-string members lose their
	// type. The two string-member set cases in the corpus compare equal, which is
	// exactly why this case is in the corpus.
	"set tag int members": "!!set members are the coerced key strings, not typed values",

	// Three inputs crash v1 outright: _read_yaml_mapping catches only
	// yaml.YAMLError, and these raise KeyError, ValueError and ValueError. Here
	// they are ordinary parse failures -- warn, and load defaults.
	"explicit bool tag y":     "v1 raises KeyError('y'); this warns and returns defaults",
	"explicit float tag word": "v1 raises ValueError; this warns and returns defaults",
	"timestamp impossible":    "v1 raises ValueError: month must be in 1..12; this warns and returns defaults",

	// PyYAML builds an infinite structure and then dies of it. Refusing is better.
	"recursive anchor":         "v1 raises RecursionError; the alias is refused instead",
	"recursive anchor mapping": "v1 raises RecursionError; the alias is refused instead",
}

func TestPyYAMLCompat(t *testing.T) {
	var corpus map[string]string
	if err := json.Unmarshal(fixture.Bytes(t, "pyyaml/corpus.json"), &corpus); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	var pin map[string]any
	if err := json.Unmarshal(fixture.Bytes(t, "pyyaml/pin.json"), &pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if len(corpus) != len(pin) {
		t.Fatalf("corpus has %d cases, pin has %d; rerun tools/oracle/pin_pyyaml.py", len(corpus), len(pin))
	}

	seen := map[string]bool{}
	for _, name := range sortedKeys(corpus) {
		want, ok := pin[name]
		if !ok {
			t.Errorf("%s: no pinned result; rerun tools/oracle/pin_pyyaml.py", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			m, err := parseYAMLMapping([]byte(corpus[name]))
			if err != nil && len(m) != 0 {
				t.Errorf("returned %d keys alongside an error: %v", len(m), err)
			}
			reason, known := knownDivergences[name]
			// The decision is reflect.DeepEqual, not cmp.Equal, and deliberately:
			// go-cmp is exponential in nesting depth on any-typed trees -- around
			// 15x per four levels, so the 20-deep case alone costs 5s. Both sides
			// here are plain JSON shapes with string leaves and no NaN, which is
			// exactly what DeepEqual compares correctly. cmp.Diff is still worth
			// its cost to explain a failure.
			equal := reflect.DeepEqual(want, tagged(m))
			switch {
			case equal && known:
				t.Errorf("listed as a known divergence (%s) but now matches PyYAML; delete the entry", reason)
			case equal:
			case known:
				seen[name] = true
			default:
				t.Errorf("differs from PyYAML (-pin +go):\n%s\ngo error: %v", cmp.Diff(want, tagged(m)), err)
			}
		})
	}

	for name := range knownDivergences {
		if !seen[name] {
			t.Errorf("known divergence %q was never exercised; is it still a corpus case?", name)
		}
	}
}

// tagged renders a value the way pin_pyyaml.py's tag() renders Python's, so a type
// change cannot compare equal to a value. It builds the same shapes
// encoding/json produces for the pin -- string leaves, []any, map[string]any --
// so cmp.Diff compares the two directly.
func tagged(v any) any {
	switch t := v.(type) {
	case nil:
		return []any{"null", ""}
	case bool:
		return []any{"bool", strconv.FormatBool(t)}
	case int64:
		return []any{"int", strconv.FormatInt(t, 10)}
	case *big.Int:
		return []any{"int", t.String()}
	case float64:
		switch {
		case math.IsNaN(t):
			return []any{"float", "nan"}
		case math.IsInf(t, 1):
			return []any{"float", "inf"}
		case math.IsInf(t, -1):
			return []any{"float", "-inf"}
		}
		// Python's repr, which for a finite float is what jsonFloat spells.
		return []any{"float", jsonFloat(t)}
	case string:
		return []any{"str", t}
	case []byte:
		return []any{"binary", string(t)}
	case time.Time:
		// datetime.isoformat(): microseconds only when non-zero, and a numeric
		// offset rather than "Z".
		layout := "2006-01-02T15:04:05-07:00"
		if t.Nanosecond() != 0 {
			layout = "2006-01-02T15:04:05.000000-07:00"
		}
		return []any{"datetime", t.Format(layout)}
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			out[k] = tagged(x)
		}
		return []any{"map", out}
	case map[string]struct{}:
		// Sorted, matching the pin's sorted-by-repr set rendering, so neither side
		// depends on map iteration order.
		out := make([]any, 0, len(t))
		for _, k := range sortedKeys(t) {
			out = append(out, tagged(k))
		}
		return []any{"set", out}
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, tagged(x))
		}
		return []any{"list", out}
	default:
		return []any{fmt.Sprintf("other:%T", v), fmt.Sprint(v)}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
