package config

// Cases the PyYAML corpus cannot reach.
//
// The implicit resolver only ever produces a handful of tags, so most of the
// constructors' own argument handling is reachable only through an explicit tag --
// and a tag naming the wrong shape (!!int on a mapping) is a document an operator
// can write but PyYAML's own test corpus has no reason to contain. The budgets have
// no PyYAML counterpart at all: they are additions, since PyYAML answers a deeply
// nested document with RecursionError.

import (
	"math"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"gopkg.in/yaml.v3"
)

func TestExplicitTags(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want map[string]any
	}{
		{"binary int", `a: !!int "0b1010"`, map[string]any{"a": int64(10)}},
		{"hex int", `a: !!int "0x1f"`, map[string]any{"a": int64(31)}},
		{"negative hex int", `a: !!int "-0x10"`, map[string]any{"a": int64(-16)}},
		{"plus hex int", `a: !!int "+0x10"`, map[string]any{"a": int64(16)}},
		{"zero", `a: !!int "0"`, map[string]any{"a": int64(0)}},
		{"negative zero", `a: !!int "-0"`, map[string]any{"a": int64(0)}},
		// int(part) per component, so a component may exceed 59 even though the
		// implicit resolver would never have matched it.
		{"loose sexagesimal", `a: !!int "1:60"`, map[string]any{"a": int64(120)}},
		{"underscored hex", `a: !!int "0x_1f"`, map[string]any{"a": int64(31)}},

		{"negative infinity", `a: !!float "-.inf"`, map[string]any{"a": math.Inf(-1)}},
		// Python's float() accepts the word, and so does Go's ParseFloat.
		{"infinity word", `a: !!float "infinity"`, map[string]any{"a": math.Inf(1)}},
		{"sexagesimal float", `a: !!float "1:30:00.5"`, map[string]any{"a": 5400.5}},
		{"float with underscores", `a: !!float "1_0.5"`, map[string]any{"a": 10.5}},

		// base64.decodebytes pads implicitly, so the "=" is optional.
		{"unpadded binary", `a: !!binary "aGk"`, map[string]any{"a": []byte("hi")}},
		{"empty binary", `a: !!binary ""`, map[string]any{"a": []byte{}}},

		{"null ignores its value", `a: !!null anything`, map[string]any{"a": nil}},
		{"bool is case-insensitive", `a: !!bool OFF`, map[string]any{"a": false}},
		{"str keeps the text", `a: !!str 0x1f`, map[string]any{"a": "0x1f"}},

		// The verbatim tag form, which is where shortTag earns its keep.
		{"verbatim tag", `a: !<tag:yaml.org,2002:int> 5`, map[string]any{"a": int64(5)}},

		{"seq of tagged", `a: !!seq [!!str 1, !!int "2"]`, map[string]any{"a": []any{"1", int64(2)}}},
		{"empty seq", `a: !!seq []`, map[string]any{"a": []any{}}},
		{"empty map", `a: !!map {}`, map[string]any{"a": map[string]any{}}},
		{"empty set", `a: !!set {}`, map[string]any{"a": map[string]struct{}{}}},
		{"empty omap", `a: !!omap []`, map[string]any{"a": []any{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseYAMLMapping([]byte(tc.yaml + "\n"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// Timestamps are compared as formatted text, not as time.Time: cmp uses
// time.Time.Equal, which considers two differently-zoned readings of the same
// instant equal, and the zone is half of what is being asserted.
func TestTimestamps(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"one-digit month and day", `a: !!timestamp 2024-1-2`, "2024-01-02T00:00:00+00:00"},
		{"utc", "a: 2024-01-02T03:04:05Z", "2024-01-02T03:04:05+00:00"},
		{"lowercase t", "a: 2024-01-02t03:04:05Z", "2024-01-02T03:04:05+00:00"},
		{"short fraction", `a: !!timestamp "2024-01-02T03:04:05.5"`, "2024-01-02T03:04:05.5+00:00"},
		// Truncated at six digits with no rounding pass, matching PyYAML 6.x.
		{"long fraction", `a: !!timestamp "2024-01-02T03:04:05.987654321"`, "2024-01-02T03:04:05.987654+00:00"},
		{"offset without minutes", `a: !!timestamp "2024-01-02T03:04:05+05"`, "2024-01-02T03:04:05+05:00"},
		{"negative offset", "a: 2024-01-02T03:04:05-08:30", "2024-01-02T03:04:05-08:30"},
		{"tab separated", `a: !!timestamp "2024-01-02 \t03:04:05\t Z"`, "2024-01-02T03:04:05+00:00"},
		{"leap day", "a: 2024-02-29", "2024-02-29T00:00:00+00:00"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseYAMLMapping([]byte(tc.yaml + "\n"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			ts, ok := got["a"].(interface{ Format(string) string })
			if !ok {
				t.Fatalf("a is %T, want a time: %#v", got["a"], got)
			}
			if s := ts.Format("2006-01-02T15:04:05.999999999-07:00"); s != tc.want {
				t.Errorf("got %s, want %s", s, tc.want)
			}
		})
	}
}

// Keys are coerced to the spelling json.dumps would have written for the Python
// value, which is what a caller of v1 could actually have observed.
func TestNonStringKeys(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"nan", "a: 1\n.nan: 2", "NaN"},
		{"negative infinity", "-.inf: 2", "-Infinity"},
		{"large float", "10000000000000000.0: 2", "1e+16"},
		{"small float", "0.00001: 2", "1e-05"},
		{"smallest fixed float", "0.0001: 2", "0.0001"},
		{"integral float", "1.0: 2", "1.0"},
		{"past int64", "9223372036854775808: 2", "9223372036854775808"},
		// json.dumps refuses both of these types outright, so the source text is a
		// stand-in for a spelling that was never observable.
		{"binary", "? !!binary aGk=\n: 2", "aGk="},
		{"timestamp", "2024-01-02: 2", "2024-01-02"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseYAMLMapping([]byte(tc.yaml + "\n"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := got[tc.want]; !ok {
				t.Errorf("no key %q; got %#v", tc.want, got)
			}
		})
	}
}

// Every one of these makes _read_yaml_mapping fall back to defaults, so the only
// thing a caller sees is the warning. The message is the whole value of the error,
// which is why it is asserted rather than just its presence.
func TestParseErrors(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{"str on a sequence", "a: !!str [1]", "expected a scalar node, but found sequence"},
		{"int on a mapping", "a: !!int {x: 1}", "expected a scalar node, but found mapping"},
		{"null on a sequence", "a: !!null [1]", "expected a scalar node, but found sequence"},
		{"bool on a sequence", "a: !!bool [1]", "expected a scalar node, but found sequence"},
		{"float on a sequence", "a: !!float [1]", "expected a scalar node, but found sequence"},
		{"binary on a mapping", "a: !!binary {}", "expected a scalar node, but found mapping"},
		{"timestamp on a sequence", "a: !!timestamp [1]", "expected a scalar node, but found sequence"},
		{"map on a sequence", "a: !!map [1]", "expected a mapping node, but found sequence"},
		{"set on a sequence", "a: !!set [1]", "expected a mapping node, but found sequence"},
		{"seq on a mapping", "a: !!seq {x: 1}", "expected a sequence node, but found mapping"},
		{"omap on a scalar", "a: !!omap 1", "expected a sequence, but found scalar"},
		{"omap item not a mapping", "a: !!omap [1]", "expected a mapping of length 1, but found scalar"},
		{"omap item a sequence", "a: !!omap [[1]]", "expected a mapping of length 1, but found sequence"},
		{"pairs item too long", "a: !!pairs [{x: 1, y: 2}]", "expected a single mapping item, but found 2 items"},

		{"empty int", `a: !!int ""`, "found an empty numeric scalar"},
		{"empty float", `a: !!float ""`, "found an empty numeric scalar"},
		{"bad binary digits", `a: !!int "0b2"`, `could not read "0b2" as a binary integer`},
		{"bad hex digits", `a: !!int "0xzz"`, `could not read "0xzz" as a hexadecimal integer`},
		{"bad octal digits", `a: !!int "09"`, `could not read "09" as an octal integer`},
		{"bad sexagesimal int", `a: !!int "1:x"`, `could not read "1:x" as a sexagesimal integer`},
		{"bad int", `a: !!int "banana"`, `could not read "banana" as an integer`},
		{"bad sexagesimal float", `a: !!float "1:x"`, `could not read "1:x" as a sexagesimal float`},
		// Go's ParseFloat takes hexadecimal floats and Python's float() does not.
		{"hex float", `a: !!float "0x1p4"`, `could not read "0x1p4" as a float`},
		{"bad bool", "a: !!bool maybe", `could not read "maybe" as a boolean`},

		{"non-ascii binary", "a: !!binary \"caf\u00e9\"", "failed to convert base64 data into ascii"},
		{"binary wrong length", `a: !!binary "a"`, "failed to decode base64 data: invalid length 1"},

		{"unparseable timestamp", "a: !!timestamp banana", `could not read "banana" as a timestamp`},
		{"hour out of range", `a: !!timestamp "2024-01-02T25:04:05"`, "field out of range"},
		{"day out of range", "a: !!timestamp 2024-02-30", "field out of range"},
		{"minute out of range", `a: !!timestamp "2024-01-02T03:99:05"`, "field out of range"},

		{"merge from a scalar in a list", "c:\n  <<: [1]", "expected a mapping for merging, but found scalar"},
		{"merge tag as a value", "a: <<", `could not determine a constructor for the tag "!!merge"`},
		{"unknown tag", "a: !nope 1", `could not determine a constructor for the tag "!nope"`},

		// v3's decoder resolves anchors, so an undefined one never reaches deref.
		{"undefined alias", "a: *nope", "unknown anchor 'nope' referenced"},
		{"recursive alias", "a: &x [*x]", "found a recursive alias"},
		{"unhashable key", "? [a]\n: 1", "found unhashable key"},

		{"second document", "a: 1\n---\nb: 2", "found another document"},
		// The second Decode fails to parse rather than returning a node, which is the
		// branch that has to distinguish a real error from io.EOF.
		{"second document malformed", "a: 1\n---\nb: [unclosed", "did not find expected"},
		{"malformed", "a: [unclosed", "did not find expected"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseYAMLMapping([]byte(tc.yaml + "\n"))
			if err == nil {
				t.Fatalf("want an error, got %#v", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is %q, want it to contain %q", err, tc.want)
			}
			if len(got) != 0 {
				t.Errorf("returned %#v alongside the error, want an empty map", got)
			}
		})
	}
}

// The budgets PyYAML does not have. Without them a config file is a denial of
// service on the process that reads it -- which for PyYAML it is, by RecursionError.
func TestBudgets(t *testing.T) {
	tests := []struct{ name, yaml, want string }{
		{
			"deeper than maxDepth",
			"a: " + strings.Repeat("[", maxDepth+50) + strings.Repeat("]", maxDepth+50),
			"exceeded the maximum nesting depth",
		},
		{
			"more nodes than maxNodes",
			"a: [" + strings.Repeat("1,", maxNodes+1) + "1]",
			"exceeded the maximum node count",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseYAMLMapping([]byte(tc.yaml + "\n"))
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error is %q, want it to contain %q", err, tc.want)
			}
			if len(got) != 0 {
				t.Errorf("returned %#v alongside the error", got)
			}
		})
	}
}

// YAML cannot anchor an alias, so the parser never hands us a chain of them and
// deref's hop limit is unreachable through parseYAMLMapping. It is still the thing
// standing between a malformed node tree and an unbounded loop, so it is tested
// against a tree built by hand.
func TestDerefRejectsAnAliasCycle(t *testing.T) {
	a := &yaml.Node{Kind: yaml.AliasNode, Value: "x"}
	b := &yaml.Node{Kind: yaml.AliasNode, Value: "x", Alias: a}
	a.Alias = b

	if _, err := deref(a); err == nil {
		t.Fatal("want an error")
	} else if !strings.Contains(err.Error(), "recursive alias") {
		t.Errorf("error is %q, want a recursive-alias error", err)
	}

	scalar := &yaml.Node{Kind: yaml.ScalarNode, Value: "1"}
	if got, err := deref(&yaml.Node{Kind: yaml.AliasNode, Alias: scalar}); err != nil || got != scalar {
		t.Errorf("deref of a single hop = %v, %v; want the target", got, err)
	}
}

func TestNodeIDOfAnUnexpectedKind(t *testing.T) {
	if got := nodeID(&yaml.Node{}); got != "unknown" {
		t.Errorf("nodeID of a zero node = %q, want %q", got, "unknown")
	}
}
