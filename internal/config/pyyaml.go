package config

// PyYAML emulation.
//
// v1 read config with yaml.safe_load, and PyYAML implements YAML *1.1*.
// gopkg.in/yaml.v3 implements roughly YAML 1.2 core. Handing a corpus of 136
// documents to both showed a naive yaml.Unmarshal agreeing with PyYAML on only
// 70 of them, so this file walks yaml.v3's parse tree with PyYAML's own resolver
// and constructors instead of trusting yaml.v3's value layer.
//
// What the naive port got wrong, and this file gets right:
//
//   - yes/Yes/YES/no/on/off are booleans, not strings.
//   - 1e3 and 1.0e3 are strings; PyYAML's float regex requires a *signed*
//     exponent. 0o17 is a string too -- YAML 1.1 spells octal 017.
//   - 017 is 15, 1:30 is 90, and 1:30.5 is 90.5 (sexagesimals).
//   - Integers past int64 keep every digit (*big.Int) instead of rounding
//     through float64.
//   - Duplicate keys silently last-win. yaml.v3's decoder rejects the document,
//     which threw away the whole file over a harmless copy-paste; decoding to a
//     yaml.Node skips that check, which is the main reason for this approach.
//   - Non-string keys are coerced the way json.dumps would have put them on the
//     wire: null, true, 1, 1.5, Infinity.
//   - !custom and !!python/object: tags make the whole file refuse to load.
//   - !!binary is bytes; !!set is a set; !!omap and !!pairs are lists of pairs.
//   - A second document makes the load fail rather than reading only the first.
//
// Deliberate divergences from v1, all recorded in differential/known_divergences.yaml:
//
//   - A !!timestamp becomes a time.Time. Python distinguishes date, naive
//     datetime and aware datetime; Go has one type, and dates and naive stamps
//     land in UTC. This cannot be observed through the JSON contract, because
//     json.dumps refuses date, datetime, bytes and set outright -- a config value
//     of any of those types could never have reached a caller as JSON.
//   - !!set becomes map[string]struct{} rather than a set of arbitrary values,
//     so members are the coerced key strings.
//   - Three inputs *crash* v1, because _read_yaml_mapping catches only
//     yaml.YAMLError and these raise something else: `!!bool y` (KeyError),
//     `!!float "banana"` (ValueError), and -- with no explicit tag at all --
//     `2024-13-45` (ValueError: month must be in 1..12). Here they are ordinary
//     parse failures: warn, and load defaults.
//   - A recursive anchor (a: &x [*x]) is refused. PyYAML builds an infinite
//     structure, which is not a behaviour worth reproducing.
//
// PyYAML has no limits of its own -- deep nesting reaches CPython's recursion
// limit and raises RecursionError straight through v1's except clause. The node,
// depth and recursion budgets below are additions, not ports.

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// Tags, in yaml.v3's short spelling.
const (
	tagNull      = "!!null"
	tagBool      = "!!bool"
	tagInt       = "!!int"
	tagFloat     = "!!float"
	tagStr       = "!!str"
	tagBinary    = "!!binary"
	tagTimestamp = "!!timestamp"
	tagValue     = "!!value"
	tagMerge     = "!!merge"
	tagMap       = "!!map"
	tagSeq       = "!!seq"
	tagOmap      = "!!omap"
	tagPairs     = "!!pairs"
	tagSet       = "!!set"
)

// Budgets PyYAML does not have. maxDepth approximates CPython's recursion limit,
// which is what stops PyYAML on a deeply nested document -- by raising, uncaught.
const (
	maxNodes     = 1 << 16
	maxDepth     = 500
	maxAliasHops = 40
)

var (
	errMultipleDocuments = errors.New("expected a single document in the stream, but found another document")
	errRecursiveAlias    = errors.New("found a recursive alias")
	errTooDeep           = errors.New("exceeded the maximum nesting depth")
	errTooManyNodes      = errors.New("exceeded the maximum node count")
	errUnhashableKey     = errors.New("found unhashable key")
	errEmptyNumber       = errors.New("found an empty numeric scalar")
)

// parseYAMLMapping is yaml.safe_load followed by v1's `data if isinstance(data,
// dict) else {}`. It never returns a nil map, so a caller that ignores the error
// still gets something indexable.
func parseYAMLMapping(data []byte) (map[string]any, error) {
	empty := map[string]any{}

	dec := yaml.NewDecoder(bytes.NewReader(data))
	var root yaml.Node
	if err := dec.Decode(&root); err != nil {
		if errors.Is(err, io.EOF) {
			// No documents at all: empty input, whitespace, or comments only.
			return empty, nil
		}
		return empty, err
	}
	// safe_load is get_single_data: a second document is an error, not something
	// to read past.
	err := dec.Decode(new(yaml.Node))
	switch {
	case err == nil:
		return empty, errMultipleDocuments
	case !errors.Is(err, io.EOF):
		return empty, err
	}
	if len(root.Content) == 0 {
		return empty, nil
	}

	b := &builder{active: map[*yaml.Node]bool{}}
	v, err := b.construct(root.Content[0], 0)
	if err != nil {
		return empty, err
	}
	m, ok := v.(map[string]any)
	if !ok {
		// Not a mapping -- a list, a scalar, or a !!set, which is deliberately not
		// a map[string]any so that a top-level set is refused the way PyYAML's
		// isinstance check refuses it.
		return empty, nil
	}
	return m, nil
}

type builder struct {
	nodes  int
	active map[*yaml.Node]bool
}

// construct is BaseConstructor.construct_object: dispatch on the effective tag.
//
// Dispatch is on the tag rather than the node kind so that a tag naming the wrong
// shape fails the way PyYAML fails it -- `!!omap {x: 1}` complains that it
// expected a sequence and found a mapping, rather than quietly reading the map.
func (b *builder) construct(n *yaml.Node, depth int) (any, error) {
	n, err := deref(n)
	if err != nil {
		return nil, err
	}
	if depth > maxDepth {
		return nil, errTooDeep
	}
	if b.nodes++; b.nodes > maxNodes {
		return nil, errTooManyNodes
	}
	// Nodes on the path from the root, so an alias pointing back into its own
	// ancestor is refused instead of expanding forever. A node reached twice by
	// separate paths is fine and stays fine.
	if b.active[n] {
		return nil, errRecursiveAlias
	}
	b.active[n] = true
	defer delete(b.active, n)

	switch tag := effectiveTag(n); tag {
	case tagNull:
		if err := requireScalar(n); err != nil {
			return nil, err
		}
		return nil, nil
	case tagBool:
		return constructBool(n)
	case tagInt:
		s, err := scalar(n)
		if err != nil {
			return nil, err
		}
		return constructInt(s)
	case tagFloat:
		s, err := scalar(n)
		if err != nil {
			return nil, err
		}
		return constructFloat(s)
	case tagStr, tagValue:
		// PyYAML constructs !!value with construct_yaml_str, so a bare `=` is the
		// string "=".
		return scalar(n)
	case tagBinary:
		s, err := scalar(n)
		if err != nil {
			return nil, err
		}
		return constructBinary(s)
	case tagTimestamp:
		s, err := scalar(n)
		if err != nil {
			return nil, err
		}
		return constructTimestamp(s)
	case tagMap:
		return b.constructMapping(n, depth)
	case tagSet:
		return b.constructSet(n, depth)
	case tagSeq:
		return b.constructSequence(n, depth)
	case tagOmap, tagPairs:
		return b.constructPairs(n, depth)
	default:
		// SafeConstructor's fallback constructor. It is what stops !!python/object:
		// and every unknown application tag, and it refuses the whole document.
		return nil, fmt.Errorf("could not determine a constructor for the tag %q", tag)
	}
}

func (b *builder) constructSequence(n *yaml.Node, depth int) ([]any, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected a sequence node, but found %s", nodeID(n))
	}
	out := make([]any, 0, len(n.Content))
	for _, item := range n.Content {
		v, err := b.construct(item, depth+1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

func (b *builder) constructMapping(n *yaml.Node, depth int) (map[string]any, error) {
	if n.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping node, but found %s", nodeID(n))
	}
	if err := b.flatten(n, depth); err != nil {
		return nil, err
	}
	out := make(map[string]any, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		kv, err := b.construct(n.Content[i], depth+1)
		if err != nil {
			return nil, err
		}
		k, err := mapKey(kv, n.Content[i])
		if err != nil {
			return nil, err
		}
		v, err := b.construct(n.Content[i+1], depth+1)
		if err != nil {
			return nil, err
		}
		// Duplicate keys last-win, silently, which is what PyYAML's dict
		// assignment does.
		out[k] = v
	}
	return out, nil
}

// constructSet is SafeConstructor.construct_yaml_set: build the mapping, keep the
// keys, discard the values.
func (b *builder) constructSet(n *yaml.Node, depth int) (map[string]struct{}, error) {
	m, err := b.constructMapping(n, depth)
	if err != nil {
		return nil, err
	}
	out := make(map[string]struct{}, len(m))
	for k := range m {
		out[k] = struct{}{}
	}
	return out, nil
}

// constructPairs serves both !!omap and !!pairs. PyYAML builds a list of
// (key, value) tuples for each, keeping duplicates and order; the difference
// between the two tags is only that an omap is meant to be unique, which PyYAML
// never checks.
func (b *builder) constructPairs(n *yaml.Node, depth int) ([]any, error) {
	if n.Kind != yaml.SequenceNode {
		return nil, fmt.Errorf("expected a sequence, but found %s", nodeID(n))
	}
	out := make([]any, 0, len(n.Content))
	for _, item := range n.Content {
		item, err := deref(item)
		if err != nil {
			return nil, err
		}
		if item.Kind != yaml.MappingNode {
			return nil, fmt.Errorf("expected a mapping of length 1, but found %s", nodeID(item))
		}
		if len(item.Content) != 2 {
			return nil, fmt.Errorf("expected a single mapping item, but found %d items", len(item.Content)/2)
		}
		k, err := b.construct(item.Content[0], depth+1)
		if err != nil {
			return nil, err
		}
		v, err := b.construct(item.Content[1], depth+1)
		if err != nil {
			return nil, err
		}
		// A tuple, so the key keeps its own type instead of being coerced to a
		// string the way a mapping key is.
		out = append(out, []any{k, v})
	}
	return out, nil
}

// flatten is SafeConstructor.flatten_mapping, mutation included.
//
// The mutation is load-bearing, not incidental: PyYAML deletes each merge pair
// from the node *before* recursing into its target, which is the only reason a
// self-referential merge (a: &x {<<: *x, y: 1}) terminates instead of recursing
// forever. Editing yaml.v3's tree in place reproduces that exactly -- and, for the
// same reason, means the recursion is bounded by the number of merge keys in the
// document rather than needing a depth guard of its own.
func (b *builder) flatten(n *yaml.Node, depth int) error {
	if depth > maxDepth {
		return errTooDeep
	}
	var merged []*yaml.Node
	for i := 0; i+1 < len(n.Content); {
		if effectiveTag(n.Content[i]) != tagMerge {
			i += 2
			continue
		}
		target, err := deref(n.Content[i+1])
		if err != nil {
			return err
		}
		n.Content = append(n.Content[:i], n.Content[i+2:]...)

		switch target.Kind {
		case yaml.MappingNode:
			if err := b.flatten(target, depth+1); err != nil {
				return err
			}
			merged = append(merged, target.Content...)
		case yaml.SequenceNode:
			pairs := make([][]*yaml.Node, 0, len(target.Content))
			for _, item := range target.Content {
				item, err := deref(item)
				if err != nil {
					return err
				}
				if item.Kind != yaml.MappingNode {
					return fmt.Errorf("expected a mapping for merging, but found %s", nodeID(item))
				}
				if err := b.flatten(item, depth+1); err != nil {
					return err
				}
				pairs = append(pairs, item.Content)
			}
			// Reversed, so that for `<<: [*l, *r]` the *earlier* mapping in the
			// list wins. Duplicate keys last-win, and reversing is what puts l's
			// pairs last among the merged ones.
			for j := len(pairs) - 1; j >= 0; j-- {
				merged = append(merged, pairs[j]...)
			}
		default:
			return fmt.Errorf("expected a mapping or list of mappings for merging, but found %s", nodeID(target))
		}
	}
	if len(merged) > 0 {
		// Merged pairs go first, so anything spelled out in the mapping itself
		// overrides them. The full slice expression forces a fresh array, since
		// a self-merge leaves merged and n.Content holding the same pointers.
		n.Content = append(merged[:len(merged):len(merged)], n.Content...)
	}
	return nil
}

// mapKey coerces a constructed key to a Go map key.
//
// PyYAML puts the key in a dict with whatever type it constructed, so a config
// file can key on None, True, 1 or .inf. Those spellings are the ones json.dumps
// would have written when v1 handed the mapping to a JSON encoder -- "null",
// "true", "1", "Infinity" -- which is why they are reproduced here rather than
// invented.
func mapKey(v any, n *yaml.Node) (string, error) {
	switch t := v.(type) {
	case nil:
		return "null", nil
	case bool:
		return strconv.FormatBool(t), nil
	case int64:
		return strconv.FormatInt(t, 10), nil
	case *big.Int:
		return t.String(), nil
	case float64:
		return jsonFloat(t), nil
	case string:
		return t, nil
	case []byte, time.Time:
		// json.dumps refuses both types outright, so no encoding of these keys was
		// ever observable. The source text is the least surprising stand-in.
		return n.Value, nil
	default:
		// A list or a mapping. PyYAML raises "found unhashable key".
		return "", errUnhashableKey
	}
}

// jsonFloat spells a float the way Python's json.dumps does: repr for finite
// values, and the JavaScript-flavoured Infinity/NaN words for the rest.
func jsonFloat(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	}
	// Python's repr switches to exponential outside [1e-4, 1e16); Go's %g picks
	// its own thresholds, so the choice is made here instead.
	if abs := math.Abs(f); abs != 0 && (abs < 1e-4 || abs >= 1e16) {
		return strconv.FormatFloat(f, 'e', -1, 64)
	}
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// --- the implicit resolver -------------------------------------------------

// PyYAML's implicit resolvers, transcribed from resolver.py with re.X whitespace
// removed, in registration order -- which is the order they are tried in, and is
// what makes 1:30.5 a float rather than tripping over the int rule.
//
// PyYAML also buckets resolvers by first character. That is a pure optimisation:
// every regex is anchored and every value it can match starts with a character in
// its own bucket, so trying them in order is equivalent.
var implicitResolvers = []struct {
	tag string
	re  *regexp.Regexp
}{
	{tagBool, regexp.MustCompile(`^(?:yes|Yes|YES|no|No|NO|true|True|TRUE|false|False|FALSE|on|On|ON|off|Off|OFF)$`)},
	{tagFloat, regexp.MustCompile(`^(?:[-+]?(?:[0-9][0-9_]*)\.[0-9_]*(?:[eE][-+][0-9]+)?` +
		`|\.[0-9][0-9_]*(?:[eE][-+][0-9]+)?` +
		`|[-+]?[0-9][0-9_]*(?::[0-5]?[0-9])+\.[0-9_]*` +
		`|[-+]?\.(?:inf|Inf|INF)` +
		`|\.(?:nan|NaN|NAN))$`)},
	{tagInt, regexp.MustCompile(`^(?:[-+]?0b[0-1_]+` +
		`|[-+]?0[0-7_]+` +
		`|[-+]?(?:0|[1-9][0-9_]*)` +
		`|[-+]?0x[0-9a-fA-F_]+` +
		`|[-+]?[1-9][0-9_]*(?::[0-5]?[0-9])+)$`)},
	{tagMerge, regexp.MustCompile(`^(?:<<)$`)},
	{tagNull, regexp.MustCompile(`^(?:~|null|Null|NULL|)$`)},
	{tagTimestamp, regexp.MustCompile(`^(?:[0-9][0-9][0-9][0-9]-[0-9][0-9]-[0-9][0-9]` +
		`|[0-9][0-9][0-9][0-9]-[0-9][0-9]?-[0-9][0-9]?(?:[Tt]|[ \t]+)` +
		`[0-9][0-9]?:[0-9][0-9]:[0-9][0-9](?:\.[0-9]*)?` +
		`(?:[ \t]*(?:Z|[-+][0-9][0-9]?(?::[0-9][0-9])?))?)$`)},
	{tagValue, regexp.MustCompile(`^(?:=)$`)},
}

// effectiveTag is Resolver.resolve, and deliberately ignores the tag yaml.v3 put
// on the node: v3 resolves plain scalars by YAML 1.2 rules, so it calls `yes` a
// string and `2024-01-02` a timestamp, and only one of those is right for us.
//
// yaml.v3 records an explicit tag by setting TaggedStyle, which is the only
// reliable way to tell `!!str 5` from a plain `5` that v3 resolved to !!int.
func effectiveTag(n *yaml.Node) string {
	if n.Style&yaml.TaggedStyle != 0 {
		return shortTag(n.Tag)
	}
	switch n.Kind {
	case yaml.ScalarNode:
		// Quoted, literal and folded scalars are never resolved implicitly --
		// PyYAML passes implicit=(False, True) for them, which lands on
		// DEFAULT_SCALAR_TAG.
		const notPlain = yaml.DoubleQuotedStyle | yaml.SingleQuotedStyle | yaml.LiteralStyle | yaml.FoldedStyle
		if n.Style&notPlain != 0 {
			return tagStr
		}
		for _, r := range implicitResolvers {
			if r.re.MatchString(n.Value) {
				return r.tag
			}
		}
		return tagStr
	case yaml.MappingNode:
		return tagMap
	case yaml.SequenceNode:
		return tagSeq
	default:
		return tagStr
	}
}

const longTagPrefix = "tag:yaml.org,2002:"

func shortTag(tag string) string {
	if strings.HasPrefix(tag, longTagPrefix) {
		return "!!" + strings.TrimPrefix(tag, longTagPrefix)
	}
	return tag
}

func nodeID(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return "scalar"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.AliasNode:
		return "alias"
	case yaml.DocumentNode:
		return "document"
	default:
		return "unknown"
	}
}

// deref follows aliases to the node they name, which is the graph PyYAML's
// composer hands the constructor: an alias is not a node of its own there.
func deref(n *yaml.Node) (*yaml.Node, error) {
	for hops := 0; n.Kind == yaml.AliasNode; hops++ {
		if n.Alias == nil {
			return nil, fmt.Errorf("found undefined alias %q", n.Value)
		}
		if hops > maxAliasHops {
			return nil, errRecursiveAlias
		}
		n = n.Alias
	}
	return n, nil
}

func requireScalar(n *yaml.Node) error {
	if n.Kind != yaml.ScalarNode {
		return fmt.Errorf("expected a scalar node, but found %s", nodeID(n))
	}
	return nil
}

func scalar(n *yaml.Node) (string, error) {
	if err := requireScalar(n); err != nil {
		return "", err
	}
	return n.Value, nil
}

// --- the scalar constructors ----------------------------------------------

var boolValues = map[string]bool{
	"yes": true, "no": false,
	"true": true, "false": false,
	"on": true, "off": false,
}

// constructBool is a map lookup on the lowercased value, so `!!bool y` fails --
// the implicit resolver never produces a bool for `y`, but an explicit tag can
// ask for one. v1 raised KeyError here and did not survive it.
func constructBool(n *yaml.Node) (bool, error) {
	s, err := scalar(n)
	if err != nil {
		return false, err
	}
	v, ok := boolValues[pytext.Lower(s)]
	if !ok {
		return false, fmt.Errorf("could not read %q as a boolean", s)
	}
	return v, nil
}

// constructInt returns an int64, or a *big.Int when the value does not fit.
// PyYAML has arbitrary-precision ints and a config file is allowed to contain
// one; rounding it through float64 the way a naive decode does would be a silent
// wrong answer.
func constructInt(s string) (any, error) {
	v := strings.ReplaceAll(s, "_", "")
	neg := false
	if v != "" && (v[0] == '-' || v[0] == '+') {
		neg = v[0] == '-'
		v = v[1:]
	}
	if v == "" {
		return nil, errEmptyNumber
	}

	n := new(big.Int)
	switch {
	case v == "0":
	case strings.HasPrefix(v, "0b"):
		if _, ok := n.SetString(v[2:], 2); !ok {
			return nil, fmt.Errorf("could not read %q as a binary integer", s)
		}
	case strings.HasPrefix(v, "0x"):
		if _, ok := n.SetString(v[2:], 16); !ok {
			return nil, fmt.Errorf("could not read %q as a hexadecimal integer", s)
		}
	case v[0] == '0':
		// PyYAML calls int(value, 8) with the leading zero still attached, and
		// Python accepts a redundant base prefix -- which is why `!!int "0o17"` is
		// 15 even though YAML 1.1 never spells octal that way. big.Int refuses the
		// prefix, so strip it.
		t := v
		if len(t) > 2 && (t[1] == 'o' || t[1] == 'O') {
			t = t[2:]
		}
		if _, ok := n.SetString(t, 8); !ok {
			return nil, fmt.Errorf("could not read %q as an octal integer", s)
		}
	case strings.Contains(v, ":"):
		// Sexagesimal: 1:30 is 90. YAML 1.1 meant it for durations.
		parts := strings.Split(v, ":")
		base := big.NewInt(1)
		sixty := big.NewInt(60)
		digit := new(big.Int)
		for i := len(parts) - 1; i >= 0; i-- {
			if _, ok := digit.SetString(parts[i], 10); !ok {
				return nil, fmt.Errorf("could not read %q as a sexagesimal integer", s)
			}
			n.Add(n, digit.Mul(digit, base))
			base.Mul(base, sixty)
		}
	default:
		if _, ok := n.SetString(v, 10); !ok {
			return nil, fmt.Errorf("could not read %q as an integer", s)
		}
	}

	if neg {
		n.Neg(n)
	}
	if n.IsInt64() {
		return n.Int64(), nil
	}
	return n, nil
}

func constructFloat(s string) (float64, error) {
	v := pytext.Lower(strings.ReplaceAll(s, "_", ""))
	neg := false
	if v != "" && (v[0] == '-' || v[0] == '+') {
		neg = v[0] == '-'
		v = v[1:]
	}
	if v == "" {
		return 0, errEmptyNumber
	}

	var f float64
	switch {
	case v == ".inf":
		f = math.Inf(1)
	case v == ".nan":
		// PyYAML ignores the sign for nan, so -.nan is nan, not -nan.
		return math.NaN(), nil
	case strings.Contains(v, ":"):
		base := 1.0
		for i, parts := 0, strings.Split(v, ":"); i < len(parts); i++ {
			part, err := strconv.ParseFloat(parts[len(parts)-1-i], 64)
			if err != nil {
				return 0, fmt.Errorf("could not read %q as a sexagesimal float", s)
			}
			f += part * base
			base *= 60
		}
	default:
		// Go's ParseFloat accepts hexadecimal floats ("0x1p4") and Python's
		// float() does not, so reject those rather than inventing a value for an
		// explicitly tagged !!float that Python would have refused.
		if strings.Contains(v, "x") {
			return 0, fmt.Errorf("could not read %q as a float", s)
		}
		parsed, err := strconv.ParseFloat(v, 64)
		if err != nil {
			return 0, fmt.Errorf("could not read %q as a float", s)
		}
		f = parsed
	}
	if neg {
		f = -f
	}
	return f, nil
}

var base64Alphabet = regexp.MustCompile(`[^A-Za-z0-9+/]`)

// constructBinary mirrors base64.decodebytes, which quietly discards anything
// outside the base64 alphabet -- so `!!binary '!!!'` decodes to no bytes at all
// rather than failing.
func constructBinary(s string) ([]byte, error) {
	for _, r := range s {
		if r > 0x7f {
			// PyYAML encodes to ASCII before decoding, and raises if it cannot.
			return nil, fmt.Errorf("failed to convert base64 data into ascii: %q", s)
		}
	}
	clean := base64Alphabet.ReplaceAllString(s, "")
	if len(clean)%4 == 1 {
		return nil, fmt.Errorf("failed to decode base64 data: invalid length %d", len(clean))
	}
	out, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("failed to decode base64 data: %w", err)
	}
	return out, nil
}

// The constructor's own regex, which is looser than the resolver's: it accepts a
// one-digit month or day, so `!!timestamp 2024-1-2` parses even though nothing
// would resolve to a timestamp implicitly.
var reTimestamp = regexp.MustCompile(`^([0-9]{4})-([0-9][0-9]?)-([0-9][0-9]?)` +
	`(?:(?:[Tt]|[ \t]+)([0-9][0-9]?):([0-9][0-9]):([0-9][0-9])(?:\.([0-9]*))?` +
	`(?:[ \t]*(Z|([-+])([0-9][0-9]?)(?::([0-9][0-9]))?))?)?$`)

// constructTimestamp builds a time.Time where PyYAML builds one of three types.
//
// A date-only value and a datetime with no zone both land at UTC. Python keeps
// them distinct -- datetime.date, and a naive datetime -- but neither can cross
// the JSON boundary, so the distinction was never observable to a caller.
//
// Out-of-range fields are refused rather than normalised: `2024-13-45` is a
// ValueError in Python, and time.Date would silently roll it into 2025-02-14.
func constructTimestamp(s string) (time.Time, error) {
	m := reTimestamp.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, fmt.Errorf("could not read %q as a timestamp", s)
	}
	year, month, day := atoi(m[1]), atoi(m[2]), atoi(m[3])

	var hour, minute, second, nsec int
	loc := time.UTC
	if m[4] != "" {
		hour, minute, second = atoi(m[4]), atoi(m[5]), atoi(m[6])
		if frac := m[7]; frac != "" {
			// Truncated to microseconds and zero-padded, with no rounding pass --
			// so .123456789 is .123456. Matches PyYAML 6.x exactly; older releases
			// rounded on the seventh digit.
			if len(frac) > 6 {
				frac = frac[:6]
			}
			nsec = atoi(frac+"000000"[:6-len(frac)]) * 1000
		}
		if m[9] != "" {
			offset := atoi(m[10])*3600 + atoi(m[11])*60
			if m[9] == "-" {
				offset = -offset
			}
			loc = time.FixedZone("", offset)
		}
	}

	t := time.Date(year, time.Month(month), day, hour, minute, second, nsec, loc)
	// time.Date normalises; Python raises. Round-tripping the fields is the
	// cheapest exact check that it did not have to.
	if t.Year() != year || int(t.Month()) != month || t.Day() != day ||
		t.Hour() != hour || t.Minute() != minute || t.Second() != second {
		return time.Time{}, fmt.Errorf("could not read %q as a timestamp: field out of range", s)
	}
	return t, nil
}

// atoi is for regex captures that are already known to be digits.
func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}
