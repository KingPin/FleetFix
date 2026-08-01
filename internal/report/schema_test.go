package report

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/fixture"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// SchemaFile is the checked-in contract, published for consumers and validated
// against here so the document and its description cannot drift apart.
const SchemaFile = "schema/fleetfix.check.v1.json"

func compiled(t *testing.T) *jsonschema.Schema {
	t.Helper()
	path := fixture.Path(SchemaFile)
	doc, err := jsonschema.UnmarshalJSON(strings.NewReader(string(fixture.Bytes(t, SchemaFile))))
	if err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource(SchemaFile, doc); err != nil {
		t.Fatalf("adding %s: %v", path, err)
	}
	sch, err := c.Compile(SchemaFile)
	if err != nil {
		t.Fatalf("%s is not a valid JSON Schema: %v", path, err)
	}
	return sch
}

func validate(t *testing.T, sch *jsonschema.Schema, rep Report) error {
	t.Helper()
	raw := mustJSON(t, rep)
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("the report is not valid JSON: %v", err)
	}
	return sch.Validate(doc)
}

// The M3 exit criterion. Every document this package can produce validates,
// including the ones that are easy to forget: the empty run, the failure, and the
// one where a check went wrong rather than the host.
func TestEveryDocumentValidatesAgainstTheCheckedInSchema(t *testing.T) {
	sch := compiled(t)

	broken := check.Result{
		ID: "docker.hygiene", Status: check.StatusError,
		Summary: "the check panicked", Error: "panic: unexpected /proc layout",
	}
	absent := check.Result{
		ID: "disk.smart", Status: check.StatusUnavailable,
		Summary: "smartctl is not installed on this host",
	}
	unrun := check.Result{
		ID: "storage.inspect", Status: check.StatusSkipped,
		Summary: "needs path, which has no default", Data: []string{"path"},
	}

	for name, rep := range map[string]Report{
		"a full run":          New(meta(), results()),
		"nothing selected":    New(meta(), nil),
		"an empty meta":       New(Meta{}, nil),
		"a run that failed":   Failed(meta(), `no check or domain named "dsk"`),
		"a broken check":      New(meta(), []check.Result{broken}),
		"an absent tool":      New(meta(), []check.Result{absent}),
		"a check nobody ran":  New(meta(), []check.Result{unrun}),
		"warnings and no run": New(withWarnings(meta()), nil),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validate(t, sch, rep); err != nil {
				t.Errorf("%v", err)
			}
		})
	}
}

// Every status the enum can hold appears in the schema, so a status added in Go
// without a schema change fails here rather than at a consumer.
func TestTheSchemaKnowsEveryStatus(t *testing.T) {
	sch := compiled(t)
	for _, s := range []check.Status{
		check.StatusOK, check.StatusWarn, check.StatusCrit,
		check.StatusSkipped, check.StatusUnavailable, check.StatusError,
	} {
		rep := New(meta(), []check.Result{{ID: "disk.usage", Status: s}})
		// New derives the envelope status from the results, so this covers the
		// envelope's enum and the result's in one pass.
		if err := validate(t, sch, rep); err != nil {
			t.Errorf("status %q: %v", s, err)
		}
	}
}

// The schema is deliberately permissive about unknown keys -- adding one is not a
// breaking change and a consumer pinned to v1 must keep validating a later
// release. That permissiveness is exactly what would let a field added in Go go
// undocumented, so the drift check is here instead, driven off the structs.
func TestEveryGoFieldIsDocumentedInTheSchema(t *testing.T) {
	raw := fixture.Bytes(t, SchemaFile)
	var doc struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
		Defs       map[string]struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}

	type block struct {
		name       string
		value      any
		required   []string
		properties map[string]json.RawMessage
	}
	blocks := []block{{"the envelope", Report{}, doc.Required, doc.Properties}}
	for def, v := range map[string]any{
		"result": check.Result{},
		"trip":   threshold.Trip{},
		"metric": check.Metric{},
		"event":  check.Event{},
	} {
		d, ok := doc.Defs[def]
		if !ok {
			t.Fatalf("$defs has no %q", def)
		}
		blocks = append(blocks, block{"$defs/" + def, v, d.Required, d.Properties})
	}
	// counts is inline rather than a $def, and its keys come from the Counts
	// struct rather than from the status enum, so it is walked like the rest.
	var counts struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(doc.Properties["counts"], &counts); err != nil {
		t.Fatal(err)
	}
	blocks = append(blocks, block{"counts", check.Counts{}, counts.Required, counts.Properties})

	for _, b := range blocks {
		typ := reflect.TypeOf(b.value)
		for i := range typ.NumField() {
			name, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
			if name == "" || name == "-" {
				continue // not on the wire; Result.Duration is the only one today
			}
			if _, ok := b.properties[name]; !ok {
				t.Errorf("%s: %s.%s is on the wire and not in the schema",
					b.name, typ.Name(), typ.Field(i).Name)
			}
			// Required as well as present: the whole no-omitempty rule is that a
			// consumer never has to test for a key, and a documented-but-optional
			// key hands that test back to them.
			if !contains(b.required, name) {
				t.Errorf("%s: %q is not in required, so a consumer must test for it",
					b.name, name)
			}
		}
		for name := range b.properties {
			if !hasWireField(typ, name) {
				t.Errorf("%s: the schema documents %q, which no longer exists in Go",
					b.name, name)
			}
		}
	}
}

// A document that lies about itself must fail. Without this the schema could be
// vacuous -- everything validates, including the thing it was written to catch --
// and every other test here would still pass.
func TestTheSchemaRejectsWhatItShould(t *testing.T) {
	sch := compiled(t)

	// disk.usage is the result carrying trips, metrics and steps. Looked up by id
	// rather than by index, because the document is sorted and a rename would turn
	// a schema assertion into a panic somewhere unrelated.
	graded := func(d map[string]any) map[string]any {
		for _, c := range d["checks"].([]any) {
			if res := c.(map[string]any); res["id"] == "disk.usage" {
				return res
			}
		}
		t.Fatal("the fixture no longer contains disk.usage")
		return nil
	}
	first := func(d map[string]any, key string) map[string]any {
		return graded(d)[key].([]any)[0].(map[string]any)
	}

	for name, mangle := range map[string]func(map[string]any){
		"a status outside the enum":       func(d map[string]any) { d["status"] = "degraded" },
		"a check status outside the enum": func(d map[string]any) { graded(d)["status"] = "degraded" },
		"an exit code that means nothing": func(d map[string]any) { d["exit_code"] = 7 },
		"the wrong schema tag":            func(d map[string]any) { d["schema"] = "fleetfix.check/v2" },
		"a dropped key":                   func(d map[string]any) { delete(d, "counts") },
		"a dropped nested key": func(d map[string]any) {
			delete(d["host"].(map[string]any), "boot_id")
		},
		"checks as an object":       func(d map[string]any) { d["checks"] = map[string]any{} },
		"a null instead of a slice": func(d map[string]any) { d["config_warnings"] = nil },
		"a timestamp without millis": func(d map[string]any) {
			d["generated_at"] = "2026-07-31T22:14:05Z"
		},
		"a local timestamp": func(d map[string]any) {
			d["generated_at"] = "2026-07-31T22:14:05.987+02:00"
		},
		"an id that is not dotted":     func(d map[string]any) { graded(d)["id"] = "diskusage" },
		"a shouty id":                  func(d map[string]any) { graded(d)["id"] = "Disk.Usage" },
		"a metric kind nobody scrapes": func(d map[string]any) { first(d, "metrics")["kind"] = "summary" },
		"a non-string label": func(d map[string]any) {
			first(d, "metrics")["labels"] = map[string]any{"mount": 1}
		},
		"a trip graded skipped":         func(d map[string]any) { first(d, "trips")["status"] = "skipped" },
		"a duration that ran backwards": func(d map[string]any) { d["duration_ms"] = -1 },
		"a step with no status":         func(d map[string]any) { delete(first(d, "steps"), "status") },
	} {
		t.Run(name, func(t *testing.T) {
			var doc map[string]any
			if err := json.Unmarshal(mustJSON(t, New(meta(), results())), &doc); err != nil {
				t.Fatal(err)
			}
			mangle(doc)
			if err := sch.Validate(doc); err == nil {
				t.Error("the schema accepted it")
			}
		})
	}
}

// An unknown key is not a break: a 2.4.0 that adds one must still validate for a
// consumer pinned to v1, which is the entire reason the schema is permissive.
func TestTheSchemaAcceptsAKeyItDoesNotKnow(t *testing.T) {
	sch := compiled(t)
	var doc map[string]any
	if err := json.Unmarshal(mustJSON(t, New(meta(), results())), &doc); err != nil {
		t.Fatal(err)
	}
	doc["something_added_in_2_4_0"] = "a value"
	doc["checks"].([]any)[0].(map[string]any)["also_new"] = 1
	if err := sch.Validate(doc); err != nil {
		t.Errorf("an additive change failed v1 validation: %v", err)
	}
}

func withWarnings(m Meta) Meta {
	m.ConfigWarnings = []string{
		`thresholds.yml: no rule named "disk.pct", ignoring it`,
		"/etc/fleetfix/probes.yml: yaml: line 3: mapping values are not allowed in this context, ignoring this file",
	}
	return m
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

func hasWireField(typ reflect.Type, name string) bool {
	for i := range typ.NumField() {
		if tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ","); tag == name {
			return true
		}
	}
	return false
}
