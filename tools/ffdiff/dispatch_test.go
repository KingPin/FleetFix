package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/KingPin/FleetFix/v2/internal/core/system"
)

func TestSystemErrNamesThePythonException(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		// Wrapped, because the readers add the file name and the field to the
		// sentinel and an == comparison here would pass on a bare sentinel while
		// failing on everything the readers actually return.
		{"short file", fmt.Errorf("uptime: %w", system.ErrShortFile), "IndexError"},
		{"bad number", fmt.Errorf("uptime: %w", system.ErrBadNumber), "ValueError"},
		{"absent", &fs.PathError{Op: "open", Path: "uptime", Err: fs.ErrNotExist}, "FileNotFoundError"},
		{"denied", &fs.PathError{Op: "open", Path: "uptime", Err: fs.ErrPermission}, "PermissionError"},
		// Not a code CPython can produce, which is the point: a failure this
		// mapping does not recognise is a bug in the port, not a reproduction of
		// v1 behaviour, and must not be able to compare equal to one.
		{"anything else", errors.New("boom"), "GoError"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := errorCode(systemErr(tt.err)); got != tt.want {
				t.Errorf("errorCode(systemErr(%v)) = %q, want %q", tt.err, got, tt.want)
			}
		})
	}
}

// TestUpdateAdaptersReturnPythonsTuple pins the wire shape of the two parsers
// that return a pair rather than a struct. Python's tuple decodes as a JSON
// array, so the Go side has to hand back one too -- an object with named fields
// would be a divergence on every case, and a bare count would silently compare
// equal to the first element of the pair.
func TestUpdateAdaptersReturnPythonsTuple(t *testing.T) {
	tests := []struct {
		name string
		fn   string
		text string
		want any
	}{
		{"notifier", "system.parse_notifier_text", "3 packages can be updated.\n5 security updates\n", []int64{3, 5}},
		// None, not (0, 0): the null is what the comparison has to see.
		{"notifier with no count", "system.parse_notifier_text", "Welcome to Ubuntu\n", nil},
		{"apt", "system.parse_apt_upgradable", "Listing...\nopenssl/jammy-security 3.0.2\n", []int64{1, 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dispatch[tt.fn].run(tt.text, nil)
			if err != nil {
				t.Fatalf("dispatch[%q] returned %v", tt.fn, err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("dispatch[%q](%q) = %#v, want %#v", tt.fn, tt.text, got, tt.want)
			}
		})
	}
}

// TestGhostAdapterKeepsPythonsFieldNames pins the wire shape of the one ported
// function that returns a list of records.
//
// py_oracle serialises a dataclass as its field names verbatim, so the JSON
// tags on GhostFile are a contract with Python rather than a Go style choice. A
// renamed field would show up in the differential as every ghost case
// diverging; here it shows up as one line naming the field.
func TestGhostAdapterKeepsPythonsFieldNames(t *testing.T) {
	v, err := dispatch["disk.parse_lsof_field_output"].run("p9\ncbash\nuroot\nf3\ns7\nL0\nn/tmp/x\n", nil)
	if err != nil {
		t.Fatalf("adapter returned %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the adapter's value: %v", err)
	}
	want := `[{"pid":9,"command":"bash","user":"root","fd":"3","size_bytes":7,"path":"/tmp/x"}]`
	if string(got) != want {
		t.Errorf("adapter marshalled to\n\t%s\nwant\n\t%s", got, want)
	}
}

// TestRecordAdaptersKeepPythonsFieldNames does the same for the other ported
// functions that return a list of dataclasses.
func TestRecordAdaptersKeepPythonsFieldNames(t *testing.T) {
	tests := []struct {
		fn   string
		text string
		want string
	}{
		{
			"docker.parse_system_df_json_lines",
			`{"Type":"Images","TotalCount":"114","Active":"6","Size":"53.5GB","Reclaimable":"45.68GB (85%)"}` + "\n",
			`[{"type":"Images","total_count":114,"active":6,"size_bytes":53500000000,` +
				`"reclaimable_bytes":45680000000,"reclaimable_pct":85}]`,
		},
		{
			// A list of raw JSON values, not of records: v1 hands back whatever the
			// line decoded to, and the big integer has to survive as itself rather
			// than as the nearest float64.
			"docker.parse_ps_json_lines",
			`{"ID":"abc","n":12345678901234567890}` + "\n",
			`[{"ID":"abc","n":12345678901234567890}]`,
		},
		{
			"services.parse_failed_units",
			"kafka.service loaded failed failed Apache Kafka\n",
			`[{"name":"kafka.service","load":"loaded","active":"failed","sub":"failed","description":"Apache Kafka"}]`,
		},
		{
			"services.parse_blame",
			"59.647s foo.service\n",
			`[{"unit":"foo.service","duration_ms":59647}]`,
		},
		// Not a dataclass, but the shape still has to be Python's list of
		// strings rather than an object keyed by unit.
		{"services.parse_show_user", "User=appuser\n", `["appuser"]`},
	}
	for _, tt := range tests {
		t.Run(tt.fn, func(t *testing.T) {
			v, err := dispatch[tt.fn].run(tt.text, nil)
			if err != nil {
				t.Fatalf("adapter returned %v", err)
			}
			got, err := json.Marshal(v)
			if err != nil {
				t.Fatalf("marshalling the adapter's value: %v", err)
			}
			if string(got) != tt.want {
				t.Errorf("adapter marshalled to\n\t%s\nwant\n\t%s", got, tt.want)
			}
		})
	}
}

// TestEnvAdapterKeepsPythonsFieldNames pins the wire shape of the one ported
// function whose result nests records inside a record, and the one whose result
// carries the path it was given.
//
// The full path in want is the load-bearing part: both oracles redact their own
// temp path by string equality, so a result carrying a basename would compare as
// a divergence on every dotenv case. It also pins the absence of "ok" -- that is
// a @property in v1 and a method here, and a field would be a key the Python side
// never emits.
func TestEnvAdapterKeepsPythonsFieldNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "e.env")
	if err := os.WriteFile(path, []byte("A=1\nA=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, err := dispatch["storage.check_env_file"].run(path, map[string]any{
		"required_keys": []any{"A", "B"},
	})
	if err != nil {
		t.Fatalf("adapter returned %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the adapter's value: %v", err)
	}
	want := `{"path":"` + path + `","exists":true,"readable":true,"keys":{"A":"2"},` +
		`"missing_required":["B"],"issues":[{"line_no":2,"raw":"A=2","message":"duplicate key: A"}]}`
	if string(got) != want {
		t.Errorf("adapter marshalled to\n\t%s\nwant\n\t%s", got, want)
	}
}

// TestSHA256AdapterReturnsPythonsNone covers the other half of a two-value return.
func TestSHA256AdapterReturnsPythonsNone(t *testing.T) {
	args := map[string]any{"asset_name": "fleetfix-linux-x86_64"}
	tests := []struct {
		name string
		text string
		want any
	}{
		{"match", "abc  fleetfix-linux-x86_64\n", "abc"},
		// None, not "": an empty digest would go on to be compared against a real
		// hash, so the two answers have to stay distinguishable on the wire.
		{"no match", "abc  some-other-file\n", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dispatch["updater.parse_sha256_line"].run(tt.text, args)
			if err != nil {
				t.Fatalf("adapter returned %v", err)
			}
			if got != tt.want {
				t.Errorf("adapter on %q = %#v, want %#v", tt.text, got, tt.want)
			}
		})
	}
}

// TestOptStrListArg covers the reader for an argument py_oracle reads with .get():
// absent is a legitimate None, but a present one that is the wrong shape is the
// two dispatch tables disagreeing and has to fail the case.
func TestOptStrListArg(t *testing.T) {
	tests := []struct {
		name    string
		args    map[string]any
		want    []string
		wantErr bool
	}{
		{name: "absent", args: map[string]any{}},
		{name: "nil args", args: nil},
		{name: "explicit null", args: map[string]any{"required_keys": nil}},
		{name: "empty list", args: map[string]any{"required_keys": []any{}}, want: []string{}},
		{
			// Order and duplicates both reach the result, so neither may be tidied.
			name: "order and duplicates",
			args: map[string]any{"required_keys": []any{"Z", "A", "A"}},
			want: []string{"Z", "A", "A"},
		},
		{name: "not a list", args: map[string]any{"required_keys": "A"}, wantErr: true},
		{name: "not strings", args: map[string]any{"required_keys": []any{"A", 1.0}}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := optStrListArg(tt.args, "required_keys")
			if (err != nil) != tt.wantErr {
				t.Fatalf("optStrListArg(%v) error = %v, wantErr %v", tt.args, err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) || (got == nil) != (tt.want == nil) {
				t.Errorf("optStrListArg(%v) = %#v, want %#v", tt.args, got, tt.want)
			}
		})
	}
}

var configFns = []string{"config.read_probes_yaml", "config.read_paths_yaml", "config.read_perf_yaml"}

// v1's loaders warn and return what they have, so a file the parser choked on is a
// value, not an error. An adapter that surfaced the error would diverge on every
// malformed fixture against a Python side that answers {}.
func TestConfigAdaptersReturnAValueNotAnError(t *testing.T) {
	dir := t.TempDir()
	malformed := filepath.Join(dir, "malformed.yml")
	if err := os.WriteFile(malformed, []byte("a: [unclosed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths := map[string]string{
		"malformed":   malformed,
		"missing":     filepath.Join(dir, "not-here.yml"),
		"unreadable":  dir, // a directory: EISDIR
		"non-mapping": writeYAML(t, dir, "list.yml", "- a\n- b\n"),
	}
	for _, fn := range configFns {
		for _, name := range []string{"malformed", "missing", "unreadable", "non-mapping"} {
			t.Run(fn+"/"+name, func(t *testing.T) {
				v, err := dispatch[fn].run(paths[name], nil)
				if err != nil {
					t.Fatalf("adapter returned %v, want the value", err)
				}
				got, err := json.Marshal(v)
				if err != nil {
					t.Fatalf("marshalling the adapter's value: %v", err)
				}
				// Not "null": v1 returns a dict the caller indexes unconditionally,
				// and a nil map would marshal as null and diverge on the
				// absent-vs-null rule rather than compare equal to {}.
				if string(got) != "{}" {
					t.Errorf("adapter marshalled to %s, want {}", got)
				}
			})
		}
	}
}

// The wire shape of a mapping that parses: ints as JSON numbers, not strings, which
// is where an int64 or a *big.Int leaking through as text would show up.
func TestConfigAdapterWireShape(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "probes.yml",
		"ping:\n  count: 5\n  interval_s: 0.3\n  targets: [10.0.0.1, 8.8.8.8]\nhuge: 12345678901234567890\n")
	v, err := dispatch["config.read_probes_yaml"].run(path, nil)
	if err != nil {
		t.Fatalf("adapter returned %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the adapter's value: %v", err)
	}
	// Go alphabetises map keys and Python preserves insertion order, which is not a
	// divergence: compare compares decoded values. The literal is written in Go's
	// order because that is what this assertion is reading.
	want := `{"huge":12345678901234567890,"ping":{"count":5,"interval_s":0.3,` +
		`"targets":["10.0.0.1","8.8.8.8"]}}`
	if string(got) != want {
		t.Errorf("adapter marshalled to\n\t%s\nwant\n\t%s", got, want)
	}
}

func writeYAML(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// No endpoint is Python's None, and the adapter must spell it null rather than a
// zero-valued object that reads like a sink configured to point nowhere.
func TestOtelAdapterSpellsNoEndpointAsNull(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "otel.yml", "service_name: svc\n")
	v, err := dispatch["audit.load_otel_config"].run(path, map[string]any{"env": map[string]any{}})
	if err != nil {
		t.Fatalf("adapter returned %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "null" {
		t.Errorf("adapter marshalled to %s, want null", got)
	}
}

// asdict on v1's frozen dataclass gives these four keys; the struct's field order
// and its never-nil headers map are what make the marshalled forms comparable.
func TestOtelAdapterWireShape(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "otel.yml", "endpoint: e\nheaders:\n  a: 1\n")
	v, err := dispatch["audit.load_otel_config"].run(path, map[string]any{
		"env": map[string]any{"FLEETFIX_OTLP_INSECURE": "1"},
	})
	if err != nil {
		t.Fatalf("adapter returned %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"endpoint":"e","headers":{"a":"1"},"insecure":true,"service_name":"fleetfix"}`
	if string(got) != want {
		t.Errorf("adapter marshalled to\n\t%s\nwant\n\t%s", got, want)
	}
}

// An empty trail must be [] and not null: py_oracle returns the list read_recent
// returns, and a nil slice would diverge on the absent-vs-null rule.
func TestRecentAdapterSpellsAnEmptyTrailAsAList(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-here.jsonl")
	v, err := dispatch["audit.read_recent"].run(missing, map[string]any{"limit": 200.0})
	if err != nil {
		t.Fatalf("adapter returned %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "[]" {
		t.Errorf("adapter marshalled to %s, want []", got)
	}
}

// Numbers keep the spelling json.loads gave them, which is the whole reason the
// records decode through UseNumber rather than into float64.
func TestRecentAdapterKeepsNumberSpellings(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "audit.jsonl", "{\"seq\":1,\"d\":1.0}\n")
	v, err := dispatch["audit.read_recent"].run(path, map[string]any{"limit": 200.0})
	if err != nil {
		t.Fatalf("adapter returned %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[{"d":1.0,"seq":1}]`; string(got) != want {
		t.Errorf("adapter marshalled to %s, want %s", got, want)
	}
}

// Both audit adapters require their argument, because py_oracle indexes args
// directly and a KeyError there has to be an error here rather than a default that
// answers a different question.
func TestAuditAdaptersRequireTheirArguments(t *testing.T) {
	path := writeYAML(t, t.TempDir(), "f", "{}\n")
	for _, fn := range []string{"audit.load_otel_config", "audit.read_recent"} {
		t.Run(fn, func(t *testing.T) {
			v, err := dispatch[fn].run(path, map[string]any{})
			if err == nil {
				t.Fatalf("adapter returned %#v, want an error", v)
			}
			if v != nil {
				t.Errorf("adapter returned %#v alongside its error, want nil", v)
			}
		})
	}
}

func TestStrMapArg(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want map[string]string
		err  bool
	}{
		{"empty", map[string]any{"env": map[string]any{}}, map[string]string{}, false},
		{
			"pairs",
			map[string]any{"env": map[string]any{"A": "1", "B": "2"}},
			map[string]string{"A": "1", "B": "2"},
			false,
		},
		// null is "no overrides", the one spelling both languages have for it.
		{"null", map[string]any{"env": nil}, map[string]string{}, false},
		{"absent", map[string]any{}, nil, true},
		{"not an object", map[string]any{"env": "A=1"}, nil, true},
		// A number here is the manifest and py_oracle disagreeing, not an env var
		// worth coercing: os.environ holds strings only.
		{"non-string value", map[string]any{"env": map[string]any{"A": 1.0}}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := strMapArg(tt.args, "env")
			if (err != nil) != tt.err {
				t.Fatalf("strMapArg(%v) error = %v, want an error: %v", tt.args, err, tt.err)
			}
			if fmt.Sprint(got) != fmt.Sprint(tt.want) {
				t.Errorf("strMapArg(%v) = %#v, want %#v", tt.args, got, tt.want)
			}
		})
	}
}

func TestNullableIntArg(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want *int64
		err  bool
	}{
		// A null and a zero are different answers here, because default_port is
		// passed through unvalidated when present.
		{"null", map[string]any{"default_port": nil}, nil, false},
		{"zero", map[string]any{"default_port": float64(0)}, ptrTo(int64(0)), false},
		{"number", map[string]any{"default_port": float64(5432)}, ptrTo(int64(5432)), false},
		{"negative", map[string]any{"default_port": float64(-5)}, ptrTo(int64(-5)), false},
		{"absent", map[string]any{}, nil, true},
		{"fractional", map[string]any{"default_port": 443.5}, nil, true},
		{"not a number", map[string]any{"default_port": "443"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := nullableIntArg(tt.args, "default_port")
			if (err != nil) != tt.err {
				t.Fatalf("nullableIntArg(%v) error = %v, want an error: %v", tt.args, err, tt.err)
			}
			switch {
			case got == nil && tt.want == nil:
			case got == nil || tt.want == nil || *got != *tt.want:
				t.Errorf("nullableIntArg(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

func ptrTo[T any](v T) *T { return &v }

func TestHostPortAdapterSpellsARejectedLineAsNull(t *testing.T) {
	// Python appends None for a line that names no target, so the list keeps its
	// alignment with the fixture's lines and a divergence names the right one.
	v, err := dispatch["net.parse_host_port"].run("h:443\n:443\nh\n", map[string]any{"default_port": nil})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `[{"host":"h","port":443},null,null]`
	if string(got) != want {
		t.Errorf("net.parse_host_port = %s, want %s", got, want)
	}
}

func TestHostPortAdapterAppliesTheDefaultPort(t *testing.T) {
	v, err := dispatch["net.parse_host_port"].run("h\n", map[string]any{"default_port": float64(5432)})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	const want = `[{"host":"h","port":5432}]`
	if string(got) != want {
		t.Errorf("net.parse_host_port = %s, want %s", got, want)
	}
}

func TestHostPortAdapterRequiresItsArgument(t *testing.T) {
	// py_oracle indexes args["default_port"] directly, so an absent one is a
	// KeyError there and must not be a silent None here.
	if _, err := dispatch["net.parse_host_port"].run("h:443\n", map[string]any{}); err == nil {
		t.Error("net.parse_host_port with no default_port argument = no error")
	}
}

func TestHostPortAdapterSpellsAnEmptyFixtureAsAList(t *testing.T) {
	// A nil slice marshals as null, which would compare unequal to Python's [].
	v, err := dispatch["net.parse_host_port"].run("\n\n", map[string]any{"default_port": nil})
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(got) != "[]" {
		t.Errorf("net.parse_host_port on blank lines = %s, want []", got)
	}
}

func TestSystemAdaptersReportAnUnreadableFileAsPythonDoes(t *testing.T) {
	// The oracle hands each adapter a real path, so a missing one is the only
	// failure reachable through the adapter rather than through the reader.
	missing := filepath.Join(t.TempDir(), "not-here")
	for _, fn := range []string{"system.read_uptime", "system.read_loadavg", "system.read_meminfo"} {
		t.Run(fn, func(t *testing.T) {
			v, err := dispatch[fn].run(missing, nil)
			if got := errorCode(err); got != "FileNotFoundError" {
				t.Errorf("dispatch[%q] on a missing file = %q, want FileNotFoundError", fn, got)
			}
			if v != nil {
				t.Errorf("dispatch[%q] returned %v alongside its error, want nil", fn, v)
			}
		})
	}
}
