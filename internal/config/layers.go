package config

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
)

// The configuration file names. An operator writes these under /etc/fleetfix or
// their own config directory; both layers use the same names, so a fleet-wide
// file and a personal override are the same document in two places.
const (
	ProbesFile     = "probes.yml"
	PathsFile      = "paths.yml"
	PerfFile       = "perf.yml"
	OtelFile       = "otel.yml"
	ThresholdsFile = "thresholds.yml"
	IdentityFile   = "identity.yml"
)

// SystemDir is the fleet-wide configuration directory, new in v2.
//
// v1 had no such layer, and the gap was not cosmetic: a cron-run `check --json`
// executes as root and reads /root/.config/fleetfix, while the operator's TUI
// reads their own home, so the same host graded thresholds differently depending
// on who asked. Ansible had nowhere to push a fleet-wide file at all.
const SystemDir = "/etc/fleetfix"

// Paths is where this process looks for configuration and writes its state.
//
// A value rather than package-level constants, because v1's were computed at
// import time from the environment -- which made them untestable and made the
// answer depend on which process imported the module first.
type Paths struct {
	// SystemDir is the fleet-wide layer; UserDir is the per-operator one.
	SystemDir string
	UserDir   string
	CacheDir  string
	StateDir  string
}

// ResolvePaths reads the XDG environment and returns this process's directories.
//
// v1 spelled each of these as
//
//	Path(os.environ.get("XDG_CONFIG_HOME", Path.home() / ".config")) / "fleetfix"
//
// which falls back only when the variable is *absent*. A variable that is set to
// the empty string returns "", so the config directory became the bare relative
// path "fleetfix" and FleetFix read ./fleetfix/probes.yml -- resolved against
// whatever the working directory happened to be. The same expression governs the
// state directory, which is where the audit log lands when /var/log is not
// writable. Measured, not inferred; see issue #11 for the table.
//
// The XDG base-directory specification says a variable that is unset *or empty*
// takes the default, and that a value which is not an absolute path must be
// ignored. This implements the specification, which is also the safe reading.
func ResolvePaths() Paths {
	home := homeDir()
	return Paths{
		SystemDir: SystemDir,
		UserDir:   xdgDir("XDG_CONFIG_HOME", home, ".config"),
		CacheDir:  xdgDir("XDG_CACHE_HOME", home, ".cache"),
		StateDir:  xdgDir("XDG_STATE_HOME", home, ".local", "state"),
	}
}

func xdgDir(env, home string, fallback ...string) string {
	if v := os.Getenv(env); filepath.IsAbs(v) {
		return filepath.Join(v, "fleetfix")
	}
	// Includes the empty and relative cases, both of which the spec calls invalid.
	return filepath.Join(append([]string{home}, append(fallback, "fleetfix")...)...)
}

// homeDir matches Path.home(): $HOME, and the passwd database when that is unset.
//
// The last resort is "/", which makes the fallback directory an absolute path
// that will simply not be readable, rather than a relative one that would read
// from the working directory -- the same failure this function exists to avoid.
func homeDir() string {
	if h := os.Getenv("HOME"); filepath.IsAbs(h) {
		return h
	}
	if u, err := user.Current(); err == nil && filepath.IsAbs(u.HomeDir) {
		return u.HomeDir
	}
	return "/"
}

// A Source is one file in the precedence chain and what became of it.
//
// Reported even when the file is absent, because `fleetfix doctor`'s job is to
// answer "which files are in play?" and a list of only the files that existed
// cannot tell an operator that the path they meant to write is not the path
// being read.
type Source struct {
	Path   string
	Exists bool
	// Err is why a layer that exists contributed nothing: it would not be read,
	// or it would not parse. Either is different from a file that contributed an
	// empty mapping, and both are different from a file that is not there.
	Err error
}

// Loaded is a merged configuration and the audit trail of where it came from.
type Loaded struct {
	// Values is the merge of every layer that parsed. Never nil, so a caller
	// that indexes it does not have to check first.
	Values map[string]any

	// Sources are the layers consulted, lowest precedence first.
	Sources []Source

	// Warnings are operator-facing lines for config_warnings[] in the report.
	Warnings []string
}

// Load reads one configuration file from every layer and merges them.
//
// Precedence, lowest first: /etc/fleetfix, then the user's config directory. The
// user layer wins, which is the direction that keeps v1's behaviour intact -- an
// operator who has ~/.config/fleetfix/probes.yml today sees exactly what they saw
// before, and /etc supplies what they have not overridden. It is also the
// familiar direction: /etc/gitconfig loses to ~/.gitconfig.
//
// Mappings merge key by key at every depth, so a fleet-wide file setting five
// probe knobs and a personal file setting one leaves the other four in force.
// Everything else replaces wholesale, lists included: concatenating a list would
// make a default target impossible to remove, and "my file says three targets but
// I get five" is a worse surprise than replacement.
//
// Never fails. A layer that will not parse contributes nothing and produces a
// warning; a fleet tool that stops reporting because of a stray tab is worse
// than one reporting on defaults.
func (p Paths) Load(name string) Loaded {
	out := Loaded{Values: map[string]any{}}
	for _, dir := range []string{p.SystemDir, p.UserDir} {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, name)
		values, exists, err := readYAMLMappingSource(path)
		out.Sources = append(out.Sources, Source{Path: path, Exists: exists, Err: err})
		switch {
		case err != nil:
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"%s: %v, ignoring this file", path, err,
			))
		case exists:
			out.Values = mergeMappings(out.Values, values)
		}
	}
	return out
}

// Effective returns the paths of the layers that actually contributed, lowest
// precedence first. This is what `doctor` prints as "in play" and what the report
// names when a threshold is not the shipped default.
func (l Loaded) Effective() []string {
	var paths []string
	for _, s := range l.Sources {
		if s.Exists && s.Err == nil {
			paths = append(paths, s.Path)
		}
	}
	return paths
}

// mergeMappings layers over onto base and returns a new map. Neither argument is
// modified: base is the accumulator from previous layers and over is a freshly
// decoded file, but a merge that mutated either would make the order of two
// independent Load calls matter.
func mergeMappings(base, over map[string]any) map[string]any {
	merged := make(map[string]any, len(base)+len(over))
	for k, v := range base {
		merged[k] = v
	}
	for k, v := range over {
		sub, isMap := v.(map[string]any)
		prev, hadMap := merged[k].(map[string]any)
		if isMap && hadMap {
			merged[k] = mergeMappings(prev, sub)
			continue
		}
		merged[k] = v
	}
	return merged
}

// DescribeLayers renders the search path for `doctor`, one line per candidate in
// precedence order. Sorted by nothing -- the order is the precedence, and sorting
// it would destroy the only information the list carries.
func (l Loaded) DescribeLayers() []string {
	lines := make([]string, 0, len(l.Sources))
	for _, s := range l.Sources {
		var unread *readError
		switch {
		case errors.As(s.Err, &unread):
			// Named apart from a parse failure: "unreadable: permission denied"
			// tells the operator to look at the mode bits, where "unparseable"
			// would send them to read a file they cannot open.
			lines = append(lines, fmt.Sprintf("%s  (unreadable: %v)", s.Path, s.Err))
		case s.Err != nil:
			lines = append(lines, fmt.Sprintf("%s  (unparseable: %v)", s.Path, s.Err))
		case s.Exists:
			lines = append(lines, fmt.Sprintf("%s  (in use)", s.Path))
		default:
			lines = append(lines, fmt.Sprintf("%s  (absent)", s.Path))
		}
	}
	return lines
}

// UnknownKeys reports top-level keys the caller does not recognise, sorted.
//
// A misspelled key in YAML is silent: the file parses, the value is stored, and
// nothing reads it -- so an operator who wrote `treshold:` sees their setting
// ignored with no indication why. Callers pass the keys they consume and put the
// result in config_warnings[].
func UnknownKeys(values map[string]any, known ...string) []string {
	set := make(map[string]struct{}, len(known))
	for _, k := range known {
		set[k] = struct{}{}
	}
	var unknown []string
	for k := range values {
		if _, ok := set[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	sort.Strings(unknown)
	return unknown
}

// Describe renders a Paths for `doctor`.
func (p Paths) Describe() string {
	return strings.Join([]string{
		"system config: " + p.SystemDir,
		"user config:   " + p.UserDir,
		"cache:         " + p.CacheDir,
		"state:         " + p.StateDir,
	}, "\n")
}
