// Package config reads FleetFix's configuration files.
//
// The YAML decoder is confined to this package, and golangci-lint's depguard
// enforces it: yaml.v3 is archived upstream, so keeping its types from leaking
// past this boundary is what makes swapping decoders a one-package change rather
// than a sweep. Every loader hands back plain Go values.
//
// The value vocabulary, chosen to match what PyYAML's SafeLoader built for v1:
//
//	nil                  !!null
//	bool                 !!bool
//	int64                !!int
//	*big.Int             !!int, when the value does not fit in an int64
//	float64              !!float
//	string               !!str
//	[]byte               !!binary
//	time.Time            !!timestamp
//	[]any                !!seq, !!omap, !!pairs
//	map[string]any       !!map
//	map[string]struct{}  !!set
//
// A loader never fails on a missing file, an unparseable one, or one whose top
// level is not a mapping -- it returns an empty map, which is v1's contract and
// what every call site is written against. It does return the parse error
// alongside, so `check --json` can carry it in config_warnings[] and `doctor` can
// show it; v1 could only log.
package config

import (
	"log/slog"
	"os"
)

// ReadProbesYAML loads probes.yml: ping targets, DNS names, HTTP URLs, the ladder.
//
// The three loaders are separate functions rather than one, as they were in v1,
// because this is where per-file validation lands: probes.yml gets range clamps,
// paths.yml gets a user lookup, perf.yml gets a bool coercion. They are identical
// today and that is not expected to last.
func ReadProbesYAML(path string) (map[string]any, error) { return readYAMLMapping(path) }

// ReadPathsYAML loads paths.yml: the inspect target and the stale-file age.
func ReadPathsYAML(path string) (map[string]any, error) { return readYAMLMapping(path) }

// ReadPerfYAML loads perf.yml: the reduce-animations override.
func ReadPerfYAML(path string) (map[string]any, error) { return readYAMLMapping(path) }

func readYAMLMapping(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the argument; naming a file is the whole call
	if err != nil {
		// v1 swallows the read error without a warning, and that is right for the
		// common case: an absent per-host override is normal, not a problem. It
		// also means an unreadable file is silent, which is why `doctor` reports
		// config-file readability separately instead of inferring it from here.
		return map[string]any{}, nil //nolint:nilerr // v1 swallows this; the returned error is reserved for a parse failure
	}
	m, err := parseYAMLMapping(data)
	if err != nil {
		// v1's _log.warning, kept. The operator's file was found and then ignored,
		// which is the one thing they need told -- silently running on defaults is
		// how a threshold override gets debugged for an hour.
		slog.Warn("failed to parse config file", "path", path, "error", err)
		return m, err
	}
	return m, nil
}
