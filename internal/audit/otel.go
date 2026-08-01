// Package audit reads and writes FleetFix's audit trail.
//
// The local JSON-lines file is authoritative. The OTLP sink is best-effort, and
// nothing in this package may let a slow or unreachable collector delay a local
// write -- see the writer for where that boundary sits.
//
// This file is the configuration half: resolving where records get shipped, which
// is a pure function of a YAML file and an environment so it can be tested and
// diffed against v1 without a collector in sight.
package audit

import (
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/config"
	"github.com/KingPin/FleetFix/v2/internal/pytext"
)

// DefaultServiceName is the resource service.name when otel.yml and the environment
// both stay quiet.
const DefaultServiceName = "fleetfix"

// Environment overrides, later-wins over otel.yml. These are a user contract: they
// are documented in v1's audit/otel.py docstring and operators have them in unit
// files, so the names are frozen.
const (
	EnvEndpoint    = "FLEETFIX_OTLP_ENDPOINT"
	EnvHeaders     = "FLEETFIX_OTLP_HEADERS"
	EnvInsecure    = "FLEETFIX_OTLP_INSECURE"
	EnvServiceName = "FLEETFIX_OTLP_SERVICE"
)

// OtelConfig is a resolved OTLP destination.
//
// Field order matches v1's frozen dataclass, because the differential harness
// compares the marshalled form. Headers is never nil: v1's default_factory made it
// {}, and a nil map would marshal as null and read as "no headers configured, and
// also something went wrong".
type OtelConfig struct {
	Endpoint    string            `json:"endpoint"`
	Headers     map[string]string `json:"headers"`
	Insecure    bool              `json:"insecure"`
	ServiceName string            `json:"service_name"`
}

// LoadOtelConfig resolves otel.yml against env, later-wins, and returns nil when no
// endpoint survives -- which is the signal to run with local-only auditing.
//
// env is passed rather than read, because a config resolver that consults the
// process environment cannot be tested at two settings in one run.
func LoadOtelConfig(path string, env map[string]string) *OtelConfig {
	// The parse error is dropped here and not plumbed onward, matching v1: the
	// loader has already warned, and a broken otel.yml means "no sink", not "no
	// audit". `doctor` reports the file's parse state from config, not from here.
	data, _ := config.ReadOtelYAML(path)

	// `data.get("endpoint") or ""` -- so a false-y value of any type means absent.
	endpoint := ""
	if s, ok := data["endpoint"].(string); ok {
		endpoint = s
	}
	// v1 applied no str() to the endpoint, so a non-string one reached the exporter
	// as whatever YAML made it and failed inside grpc, several frames from the
	// mistake. Treated as absent here -- an OTLP endpoint that is not a string is not
	// an endpoint -- which leaves the environment layer below still able to supply
	// one, where an early return would not.

	headers := map[string]string{}
	if raw, ok := data["headers"].(map[string]any); ok {
		// v1 coerces both sides with str(), so `x-token: 12345` unquoted still gets
		// through as "12345".
		for k, v := range raw {
			headers[k] = config.PyStr(v)
		}
	}

	insecure := config.PyTruthy(data["insecure"])

	serviceName := DefaultServiceName
	if v := data["service_name"]; config.PyTruthy(v) {
		serviceName = config.PyStr(v)
	}

	if v := env[EnvEndpoint]; v != "" {
		endpoint = v
	}
	if v := env[EnvHeaders]; v != "" {
		// Merged over the file's headers, not replacing them.
		for k, hv := range parseHeadersEnv(v) {
			headers[k] = hv
		}
	}
	if env[EnvInsecure] != "" {
		// Any non-empty value, including "false" -- documented as such in v1, and
		// unit files in the field say "1".
		insecure = true
	}
	if v := env[EnvServiceName]; v != "" {
		serviceName = v
	}

	if endpoint == "" {
		return nil
	}
	return &OtelConfig{
		Endpoint:    endpoint,
		Headers:     headers,
		Insecure:    insecure,
		ServiceName: serviceName,
	}
}

// parseHeadersEnv reads the "k1=v1,k2=v2" form. A pair with no "=" is skipped
// rather than treated as a valueless header, and every part is stripped, so
// "a = 1, b=2" is the two headers it looks like.
func parseHeadersEnv(raw string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(raw, ",") {
		pair = strings.TrimFunc(pair, pytext.IsSpace)
		k, v, ok := strings.Cut(pair, "=")
		if pair == "" || !ok {
			continue
		}
		out[strings.TrimFunc(k, pytext.IsSpace)] = strings.TrimFunc(v, pytext.IsSpace)
	}
	return out
}
