package audit

// The json.dumps compatibility suite.
//
// testdata/json_dumps/pin.json holds the exact string
// json.dumps(value, ensure_ascii=False) returned for each of the values in
// testdata/json_dumps/corpus.json, measured by running it -- see
// tools/oracle/pin_audit_json.py. This test replays the corpus through the audit
// encoder and requires the same bytes.
//
// It is not part of the differential harness. The manifest holds no audit records
// at all, because v1 wrote the trail as a side effect of destructive actions the
// harness must not perform -- and the pin has to outlive the Python, which is
// deleted at M7.

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

func TestJSONDumpsCompat(t *testing.T) {
	var corpus map[string][]any
	if err := json.Unmarshal(fixture.Bytes(t, "json_dumps/corpus.json"), &corpus); err != nil {
		t.Fatalf("corpus: %v", err)
	}
	var pin map[string]string
	if err := json.Unmarshal(fixture.Bytes(t, "json_dumps/pin.json"), &pin); err != nil {
		t.Fatalf("pin: %v", err)
	}
	if len(corpus) != len(pin) {
		t.Fatalf("corpus has %d cases, pin has %d; rerun tools/oracle/pin_audit_json.py", len(corpus), len(pin))
	}

	for name, tagged := range corpus {
		want, ok := pin[name]
		if !ok {
			t.Errorf("%s: no pinned result; rerun tools/oracle/pin_audit_json.py", name)
			continue
		}
		t.Run(name, func(t *testing.T) {
			var b strings.Builder
			writeValue(&b, untag(t, tagged))
			if got := b.String(); got != want {
				t.Errorf("encoded %q\n     python %q", got, want)
			}
		})
	}
}

// untag rebuilds the Go value from the corpus's [kind, spelling] pair.
//
// The kinds are the ones pin_audit_json.py emits, and an unknown one is fatal
// rather than skipped: a corpus case the Go side cannot express is a hole in the
// suite, not a pass.
func untag(t *testing.T, tagged []any) any {
	t.Helper()
	if len(tagged) != 2 {
		t.Fatalf("malformed corpus entry %v", tagged)
	}
	kind, ok := tagged[0].(string)
	if !ok {
		t.Fatalf("malformed corpus kind %v", tagged[0])
	}
	switch kind {
	case "null":
		return nil
	case "bool":
		return tagged[1] == "true"
	case "int":
		n, err := strconv.ParseInt(str(t, tagged[1]), 10, 64)
		if err != nil {
			t.Fatalf("corpus int %v: %v", tagged[1], err)
		}
		return n
	case "float":
		switch s := str(t, tagged[1]); s {
		case "nan":
			return math.NaN()
		case "inf":
			return math.Inf(1)
		case "-inf":
			return math.Inf(-1)
		default:
			f, err := strconv.ParseFloat(s, 64)
			if err != nil {
				t.Fatalf("corpus float %q: %v", s, err)
			}
			return f
		}
	case "str":
		return str(t, tagged[1])
	case "list":
		items, ok := tagged[1].([]any)
		if !ok {
			t.Fatalf("corpus list %v", tagged[1])
		}
		out := make([]any, 0, len(items))
		for _, e := range items {
			out = append(out, untag(t, elem(t, e)))
		}
		return out
	case "map":
		pairs, ok := tagged[1].([]any)
		if !ok {
			t.Fatalf("corpus map %v", tagged[1])
		}
		out := make(Fields, 0, len(pairs))
		for _, p := range pairs {
			kv := elem(t, p)
			if len(kv) != 2 {
				t.Fatalf("malformed corpus pair %v", p)
			}
			out = append(out, Field{Key: str(t, kv[0]), Value: untag(t, elem(t, kv[1]))})
		}
		return out
	default:
		t.Fatalf("corpus kind %q has no Go equivalent", kind)
		return nil
	}
}

func str(t *testing.T, v any) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("expected a corpus string, got %T", v)
	}
	return s
}

func elem(t *testing.T, v any) []any {
	t.Helper()
	e, ok := v.([]any)
	if !ok {
		t.Fatalf("expected a corpus pair, got %T", v)
	}
	return e
}

// The empty-list and empty-map cases in the corpus arrive as []any and Fields, but
// a caller can also hand the encoder a []string, which is the shape a list of
// units or package names has.
func TestWriteValueRendersAStringSlice(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []string
		want string
	}{
		{"empty", []string{}, "[]"},
		{"one", []string{"nginx"}, `["nginx"]`},
		{"escaped", []string{"a\nb", `c"d`}, `["a\nb", "c\"d"]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			writeValue(&b, tc.in)
			if got := b.String(); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

// A field whose type the encoder does not know is rendered rather than dropped or
// refused, because the record is written while something is being done to a host
// and both alternatives lose more than a misspelt value does.
func TestWriteValueRendersAnUnexpectedTypeAsAString(t *testing.T) {
	type unitID struct {
		Name string
		N    int
	}
	var b strings.Builder
	writeValue(&b, unitID{Name: "nginx", N: 2})
	if got, want := b.String(), `"{nginx 2}"`; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// An int, unlike an int64, is what a caller writes without thinking about it.
func TestWriteValueRendersAPlainInt(t *testing.T) {
	var b strings.Builder
	writeValue(&b, 42)
	if got, want := b.String(), "42"; got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

// Go strings can hold bytes Python's str cannot. The trail names the file that was
// actually touched, so the bytes pass through rather than becoming U+FFFD.
func TestWriteStringPassesInvalidUTF8Through(t *testing.T) {
	var b strings.Builder
	writeString(&b, "bad\xff\xfename")
	if got, want := b.String(), "\"bad\xff\xfename\""; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// emptyAsNull is the one place the in-process vocabulary and the wire vocabulary
// are allowed to differ, so it is worth saying which way round.
func TestEmptyAsNull(t *testing.T) {
	if got := emptyAsNull(""); got != nil {
		t.Errorf("empty became %v, want nil", got)
	}
	if got := emptyAsNull("duo:jdoe"); got != "duo:jdoe" {
		t.Errorf("got %v, want the string through", got)
	}
}

// A record with no result at all and a record whose result is empty are different
// facts: the first is an intent line or a lifecycle event, the second is an action
// that finished having recorded nothing.
func TestMarshalRecordSeparatesAbsentResultFromEmptyResult(t *testing.T) {
	rec := Record{Phase: PhaseIntent}
	if got := string(marshalRecord(rec)); !strings.Contains(got, `"result": null`) {
		t.Errorf("absent result rendered as %s", got)
	}
	rec.HasResult = true
	if got := string(marshalRecord(rec)); !strings.Contains(got, `"result": {}`) {
		t.Errorf("empty result rendered as %s", got)
	}
}

// Fields is an ordered list rather than a map because Python's dict.update
// replaces a value where the key already sits. A Go map would reorder the line,
// and a consumer diffing two hosts' trails would see every field move.
func TestFieldsSetReplacesInPlaceAndAppendsOtherwise(t *testing.T) {
	var f Fields
	f.Set("ok", true)
	f.Set("error", nil)
	f.Set("freed_bytes", 10)
	f.Set("ok", false)

	var got []string
	for _, m := range f {
		got = append(got, fmt.Sprintf("%s=%v", m.Key, m.Value))
	}
	want := "ok=false error=<nil> freed_bytes=10"
	if strings.Join(got, " ") != want {
		t.Errorf("got %q, want %q", strings.Join(got, " "), want)
	}
}
