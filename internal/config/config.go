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
// A loader never fails on a missing file, an unreadable one, an unparseable one,
// or one whose top level is not a mapping -- it returns an empty map, which is
// v1's contract and what every call site is written against. It does return the
// read or parse error alongside, so `check --json` can carry it in
// config_warnings[] and `doctor` can show it; v1 could only log.
//
// "Never fails" is stronger here than in v1, deliberately. v1 read the file with
// Path.read_text(encoding="utf-8") and caught OSError, so a config file containing
// invalid UTF-8 raised UnicodeDecodeError past the handler and took the process
// down at startup. Here it is an ordinary parse failure: warn, and load defaults.
package config

import (
	"errors"
	"io/fs"
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

// ReadOtelYAML loads otel.yml: the OTLP endpoint, headers, insecure and service name.
//
// v1 kept a second copy of the loader in audit/otel.py rather than importing
// config's, and the two were byte-for-byte the same three steps -- read, safe_load,
// keep it only if it is a dict -- so this collapses them into one. internal/audit
// resolves the values; reading the file is this package's job, since the decoder
// lives here.
func ReadOtelYAML(path string) (map[string]any, error) { return readYAMLMapping(path) }

func readYAMLMapping(path string) (map[string]any, error) {
	m, _, err := readYAMLMappingSource(path)
	return m, err
}

// A readError is a layer that was there and could not be read: a permission bit,
// a directory where a file was meant to go, an I/O error.
//
// Told apart from a parse failure because the operator's fix is a different one
// -- chmod, not YAML -- and because "absent" is the wrong thing to tell them
// either way. It carries the bare cause rather than os.ReadFile's *fs.PathError,
// since every line that renders one already names the path.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

func newReadError(err error) *readError {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) {
		return &readError{err: pathErr.Err}
	}
	return &readError{err: err}
}

// readYAMLMappingSource is readYAMLMapping plus whether the file was there.
//
// The layered loader needs that third answer and the v1-compatible readers must
// not grow it: a file that is absent and a file that holds an empty mapping are
// the same thing to a v1 call site, but to a merge they are not. An absent layer
// contributes nothing; a present but empty one is an operator saying "nothing
// here", which is worth reporting as in-use rather than as missing.
func readYAMLMappingSource(path string) (values map[string]any, exists bool, err error) {
	data, err := os.ReadFile(path) //nolint:gosec // the path is the argument; naming a file is the whole call
	switch {
	case errors.Is(err, fs.ErrNotExist):
		// The common case and not a problem: most hosts have no per-host
		// override, and v1 swallowed this silently for exactly that reason.
		return map[string]any{}, false, nil //nolint:nilerr // absence is the normal case, not a finding
	case err != nil:
		// Found and then not read. v1 swallowed this along with absence, which is
		// how a root-owned /etc/fleetfix/thresholds.yml gets debugged for an hour:
		// the operator's file is right there and the report says nothing. Reported
		// as a layer that exists and contributed nothing, so the audit can say
		// "found but could not be read" rather than "absent".
		slog.Warn("failed to read config file", "path", path, "error", err)
		return map[string]any{}, true, newReadError(err)
	}
	m, err := parseYAMLMapping(data)
	if err != nil {
		// v1's _log.warning, kept. The operator's file was found and then ignored,
		// which is the one thing they need told -- silently running on defaults is
		// how a threshold override gets debugged for an hour.
		slog.Warn("failed to parse config file", "path", path, "error", err)
		return m, true, err
	}
	return m, true, nil
}
