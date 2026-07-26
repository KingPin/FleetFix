package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/KingPin/FleetFix/v2/internal/fixture"
)

func TestLoadOtelConfigFromYAML(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    *OtelConfig
	}{
		{
			"every key", "otel/full.yml",
			&OtelConfig{
				Endpoint:    "https://ingest.example.com:443",
				Headers:     map[string]string{"x-otlp-token": "abc"},
				Insecure:    false,
				ServiceName: "fleetfix-test",
			},
		},
		{
			// No service_name and no insecure: the defaults, and the one that is not
			// the zero value has to come from somewhere.
			"endpoint and headers", "otel/endpoint_and_headers.yml",
			&OtelConfig{
				Endpoint:    "https://ingest.example.com:443",
				Headers:     map[string]string{"a": "1"},
				ServiceName: DefaultServiceName,
			},
		},
		// A file that does not parse and a file that is not a mapping both reduce to
		// no endpoint, which is local-only auditing rather than an error.
		{"unparseable", "otel/malformed.yml", nil},
		{"not a mapping", "otel/top_level_list.yml", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := LoadOtelConfig(fixture.Path(tc.fixture), map[string]string{})
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestLoadOtelConfigMissingFileIsLocalOnly(t *testing.T) {
	if got := LoadOtelConfig(filepath.Join(t.TempDir(), "nope.yml"), nil); got != nil {
		t.Errorf("got %#v, want nil", got)
	}
}

// The environment is the later-wins layer, and "wins" differs per key: endpoint and
// service_name replace, headers merge, insecure only ever turns on.
func TestLoadOtelConfigEnvOverrides(t *testing.T) {
	full := fixture.Path("otel/full.yml")
	tests := []struct {
		name string
		path string
		env  map[string]string
		want *OtelConfig
	}{
		{
			"endpoint replaces", full,
			map[string]string{EnvEndpoint: "http://localhost:4317"},
			&OtelConfig{"http://localhost:4317", map[string]string{"x-otlp-token": "abc"}, false, "fleetfix-test"},
		},
		{
			"headers merge, not replace", full,
			map[string]string{EnvHeaders: "x-tenant=acme"},
			&OtelConfig{
				"https://ingest.example.com:443",
				map[string]string{"x-otlp-token": "abc", "x-tenant": "acme"},
				false, "fleetfix-test",
			},
		},
		{
			"a header of the same name wins", full,
			map[string]string{EnvHeaders: "x-otlp-token=xyz"},
			&OtelConfig{
				"https://ingest.example.com:443",
				map[string]string{"x-otlp-token": "xyz"},
				false, "fleetfix-test",
			},
		},
		{
			// Documented as "any non-empty value", which includes the ones that read
			// as off. An operator who writes FLEETFIX_OTLP_INSECURE=false gets
			// insecure, and that has been true since v1.
			"insecure is any non-empty value", full,
			map[string]string{EnvInsecure: "false"},
			&OtelConfig{
				"https://ingest.example.com:443",
				map[string]string{"x-otlp-token": "abc"},
				true, "fleetfix-test",
			},
		},
		{
			"empty insecure does not turn it on", full,
			map[string]string{EnvInsecure: ""},
			&OtelConfig{
				"https://ingest.example.com:443",
				map[string]string{"x-otlp-token": "abc"},
				false, "fleetfix-test",
			},
		},
		{
			"service name replaces", full,
			map[string]string{EnvServiceName: "fleetfix-agent"},
			&OtelConfig{
				"https://ingest.example.com:443",
				map[string]string{"x-otlp-token": "abc"},
				false, "fleetfix-agent",
			},
		},
		{
			// The env layer alone is a complete configuration -- the documented way to
			// run with no otel.yml at all.
			"endpoint from env with no file", filepath.Join(t.TempDir(), "nope.yml"),
			map[string]string{
				EnvEndpoint:    "http://localhost:4317",
				EnvHeaders:     "a=1,b=2",
				EnvInsecure:    "1",
				EnvServiceName: "svc",
			},
			&OtelConfig{"http://localhost:4317", map[string]string{"a": "1", "b": "2"}, true, "svc"},
		},
		{
			// Headers without an endpoint is still local-only: there is nowhere to
			// send them.
			"headers alone are not a destination", filepath.Join(t.TempDir(), "nope.yml"),
			map[string]string{EnvHeaders: "a=1"},
			nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, LoadOtelConfig(tc.path, tc.env)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// v1 coerces both sides of the headers mapping with str(), which is what lets an
// unquoted token through. Everything else in the file is read for its type.
func TestLoadOtelConfigCoercions(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want *OtelConfig
	}{
		{
			"unquoted header values", "endpoint: e\nheaders:\n  n: 12345\n  f: 1.5\n  b: yes\n  z: ~\n",
			&OtelConfig{"e", map[string]string{"n": "12345", "f": "1.5", "b": "True", "z": "None"}, false, DefaultServiceName},
		},
		{
			"non-string service name", "endpoint: e\nservice_name: 123\n",
			&OtelConfig{"e", map[string]string{}, false, "123"},
		},
		{
			// bool() of a non-empty string, which a quoted "false" is. The operator
			// meant the opposite; v1 did this too.
			"quoted false is insecure", "endpoint: e\ninsecure: 'false'\n",
			&OtelConfig{"e", map[string]string{}, true, DefaultServiceName},
		},
		{
			// Unquoted, the YAML 1.1 resolver makes it a bool, and then it means what
			// it says.
			"unquoted no is not insecure", "endpoint: e\ninsecure: no\n",
			&OtelConfig{"e", map[string]string{}, false, DefaultServiceName},
		},
		{"zero is not insecure", "endpoint: e\ninsecure: 0\n", &OtelConfig{"e", map[string]string{}, false, DefaultServiceName}},
		{"empty list is not insecure", "endpoint: e\ninsecure: []\n", &OtelConfig{"e", map[string]string{}, false, DefaultServiceName}},
		{"non-empty list is insecure", "endpoint: e\ninsecure: [a]\n", &OtelConfig{"e", map[string]string{}, true, DefaultServiceName}},
		{
			// `data.get("headers") or {}` then an isinstance check, so a headers key
			// that is not a mapping is no headers at all.
			"headers that are not a mapping", "endpoint: e\nheaders: [a, b]\n",
			&OtelConfig{"e", map[string]string{}, false, DefaultServiceName},
		},
		{
			// A falsy service_name falls back rather than being coerced, so it is the
			// default and not "".
			"empty service name", "endpoint: e\nservice_name: ''\n",
			&OtelConfig{"e", map[string]string{}, false, DefaultServiceName},
		},
		{"empty endpoint", "endpoint: ''\n", nil},
		{"null endpoint", "endpoint: ~\n", nil},
		// v1 handed a non-string endpoint to the exporter and failed inside grpc.
		{"non-string endpoint", "endpoint: 123\n", nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "otel.yml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0o600); err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, LoadOtelConfig(path, nil)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// A non-string endpoint must not be able to reach the env layer's decision either
// way round -- it is absent, so the env still supplies one.
func TestLoadOtelConfigNonStringEndpointStillTakesTheEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "otel.yml")
	if err := os.WriteFile(path, []byte("endpoint: 123\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := LoadOtelConfig(path, map[string]string{EnvEndpoint: "http://localhost:4317"})
	want := &OtelConfig{"http://localhost:4317", map[string]string{}, false, DefaultServiceName}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
}

func TestParseHeadersEnv(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want map[string]string
	}{
		{"one pair", "a=1", map[string]string{"a": "1"}},
		{"two pairs", "a=1,b=2", map[string]string{"a": "1", "b": "2"}},
		{"stripped", " a = 1 , b = 2 ", map[string]string{"a": "1", "b": "2"}},
		// Skipped, not read as a valueless header: a bare word in a k=v list is a
		// typo, and inventing "" for it would ship a header the operator never wrote.
		{"no equals", "a=1,nope,b=2", map[string]string{"a": "1", "b": "2"}},
		{"empty pairs", "a=1,,b=2,", map[string]string{"a": "1", "b": "2"}},
		{"empty string", "", map[string]string{}},
		{"only commas", ",,,", map[string]string{}},
		// Split on the first "=" only, so a base64 or URL value survives.
		{"value contains equals", "a=b=c", map[string]string{"a": "b=c"}},
		{"empty value", "a=", map[string]string{"a": ""}},
		{"empty key", "=1", map[string]string{"": "1"}},
		{"last of a repeated key wins", "a=1,a=2", map[string]string{"a": "2"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if diff := cmp.Diff(tc.want, parseHeadersEnv(tc.raw)); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

// Field order and the never-null headers map are the differential's contract, so
// they are asserted on the marshalled bytes rather than the struct.
func TestOtelConfigWireShape(t *testing.T) {
	got, err := json.Marshal(LoadOtelConfig(fixture.Path("otel/full.yml"), nil))
	if err != nil {
		t.Fatal(err)
	}
	want := `{"endpoint":"https://ingest.example.com:443","headers":{"x-otlp-token":"abc"},` +
		`"insecure":false,"service_name":"fleetfix-test"}`
	if string(got) != want {
		t.Errorf("marshalled to\n\t%s\nwant\n\t%s", got, want)
	}

	// No endpoint marshals as null, matching Python's None, not as a zero-valued
	// object that reads like a configured sink pointing nowhere.
	got, err = json.Marshal(LoadOtelConfig(fixture.Path("otel/malformed.yml"), nil))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "null" {
		t.Errorf("marshalled to %s, want null", got)
	}
}

// Headers must be {} and never null, even with no headers anywhere.
func TestOtelConfigHeadersAreNeverNull(t *testing.T) {
	got := LoadOtelConfig(filepath.Join(t.TempDir(), "nope.yml"), map[string]string{EnvEndpoint: "e"})
	if got.Headers == nil {
		t.Fatal("Headers is nil; it must marshal as {}")
	}
	b, err := json.Marshal(got.Headers)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{}" {
		t.Errorf("Headers marshalled to %s, want {}", b)
	}
}
