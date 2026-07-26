package main

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/KingPin/FleetFix/v2/internal/core/disk"
	"github.com/KingPin/FleetFix/v2/internal/core/docker"
	"github.com/KingPin/FleetFix/v2/internal/core/logsqueeze"
	"github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/core/services"
	"github.com/KingPin/FleetFix/v2/internal/core/storage"
	"github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/updater"
)

// adapter is one entry in the manifest's language-neutral function namespace.
//
// The manifest cannot name a Go function any more than it can name a Python
// method, so each side hand-writes the same table against the same names. input
// mirrors the manifest's "text" or "path" and is checked rather than assumed: a
// mismatch means the two tables have drifted, which is worth a hard failure
// while it is still one line rather than a silent wrong-payload result.
type adapter struct {
	input string
	run   func(payload string, args map[string]any) (any, error)
}

// text wraps an adapter that takes the fixture's contents.
func text(fn func(string, map[string]any) (any, error)) adapter {
	return adapter{input: "text", run: fn}
}

// pathFS wraps an adapter for a reader that opens its own input.
//
// v1 takes a Path and the port takes an fs.FS plus a name, so the temp file the
// oracle wrote is split into its directory and basename here rather than every
// reader growing a path-shaped overload it has no other caller for.
func pathFS(fn func(fs.FS, string, map[string]any) (any, error)) adapter {
	return adapter{input: "path", run: func(p string, args map[string]any) (any, error) {
		return fn(os.DirFS(filepath.Dir(p)), filepath.Base(p), args)
	}}
}

// wholePath wraps an adapter for a function that takes a path and reports it back.
//
// The path must reach it undivided. Both oracles redact their own temp path by
// comparing result strings against it for equality, so a function handed a
// basename returns a basename, nothing matches the redaction, and every case for
// it diverges on a difference that is only the tempdir's name.
func wholePath(fn func(string, map[string]any) (any, error)) adapter {
	return adapter{input: "path", run: fn}
}

// dispatch mirrors py_oracle.py's DISPATCH, one entry per ported function.
//
// Entries appear as their function lands. A manifest case with no entry here
// records UnknownFunction and is counted by `compare` as unimplemented, so the
// gap between this table and py_oracle's is the port's remaining parser work,
// visible on every run instead of tracked in someone's head.
var dispatch = map[string]adapter{
	// disk
	"disk.parse_df": text(func(t string, _ map[string]any) (any, error) {
		return disk.ParseDF(t), nil
	}),
	"disk.parse_df_inodes": text(func(t string, _ map[string]any) (any, error) {
		return disk.ParseDFInodes(t), nil
	}),
	"disk.parse_health": text(func(t string, _ map[string]any) (any, error) {
		// Python returns None when the line is absent. Returning "" instead
		// would land on the null-vs-empty cosmetic rule and be normalised away,
		// so the one case where the two really could disagree would stop being
		// visible. Return the null.
		if v, ok := disk.ParseHealth(t); ok {
			return v, nil
		}
		return nil, nil
	}),
	"disk.parse_sata_attributes": text(func(t string, _ map[string]any) (any, error) {
		return disk.ParseSATAAttributes(t), nil
	}),
	"disk.parse_nvme_attributes": text(func(t string, _ map[string]any) (any, error) {
		return disk.ParseNVMeAttributes(t), nil
	}),
	"disk.parse_lsof_field_output": text(func(t string, _ map[string]any) (any, error) {
		return disk.ParseLsofFieldOutput(t), nil
	}),

	// docker
	"docker.parse_ps_json_lines": text(func(t string, _ map[string]any) (any, error) {
		return docker.ParsePSJSONLines(t), nil
	}),
	"docker.parse_system_df_json_lines": text(func(t string, _ map[string]any) (any, error) {
		return docker.ParseSystemDFJSONLines(t), nil
	}),
	"docker.parse_reclaimed_total": text(func(t string, _ map[string]any) (any, error) {
		return docker.ParseReclaimedTotal(t), nil
	}),

	// logsqueeze
	"logsqueeze.lsof_has_writer": text(func(t string, _ map[string]any) (any, error) {
		return logsqueeze.LsofHasWriter(t), nil
	}),

	// network
	"net.parse_ping_output": text(func(t string, args map[string]any) (any, error) {
		target, err := strArg(args, "target")
		if err != nil {
			return nil, err
		}
		// Python returns None when the summary is absent; the zero PingSummary
		// would compare as a real reading of zero packets.
		if v, ok := network.ParsePingOutput(target, t); ok {
			return v, nil
		}
		return nil, nil
	}),
	"net.parse_traceroute_output": text(func(t string, args map[string]any) (any, error) {
		return traceArgs(args, t, network.ParseTracerouteOutput)
	}),
	"net.parse_tracepath_output": text(func(t string, args map[string]any) (any, error) {
		return traceArgs(args, t, network.ParseTracepathOutput)
	}),
	"net.parse_ss_output": text(func(t string, _ map[string]any) (any, error) {
		return network.ParseSSOutput(t), nil
	}),
	"net.parse_curl_output": text(func(t string, args map[string]any) (any, error) {
		url, err := strArg(args, "url")
		if err != nil {
			return nil, err
		}
		// Python returns None when the template is absent or unusable.
		if v, ok := network.ParseCurlOutput(url, t); ok {
			return v, nil
		}
		return nil, nil
	}),
	"net.parse_resolv_conf": text(func(t string, _ map[string]any) (any, error) {
		// source is keyword-only with a "" default and py_oracle leaves it there,
		// so the manifest carries no argument for it and neither side is naming a
		// path. Pass the same default rather than inventing one here.
		return network.ParseResolvConf(t, ""), nil
	}),
	"net.default_route": pathFS(func(fsys fs.FS, name string, _ map[string]any) (any, error) {
		// Python returns an (iface, gateway) tuple, or None when there is no
		// default route.
		if iface, gateway, ok := network.DefaultRoute(fsys, name); ok {
			return []string{iface, gateway}, nil
		}
		return nil, nil
	}),
	"net.read_counters": pathFS(func(fsys fs.FS, name string, _ map[string]any) (any, error) {
		// Python's values are (rx, tx) tuples. Flattening the struct back to the
		// pair here keeps the named fields the rest of the port reads, without
		// asking the comparison to treat an object and an array as the same thing.
		out := map[string][]int64{}
		for iface, c := range network.ReadCounters(fsys, name) {
			out[iface] = []int64{c.RxBytes, c.TxBytes}
		}
		return out, nil
	}),

	// services
	"services.parse_failed_units": text(func(t string, _ map[string]any) (any, error) {
		return services.ParseFailedUnits(t), nil
	}),
	"services.parse_show_user": text(func(t string, _ map[string]any) (any, error) {
		return services.ParseShowUser(t), nil
	}),
	"services.parse_blame": text(func(t string, _ map[string]any) (any, error) {
		return services.ParseBlame(t), nil
	}),

	// storage
	"storage.check_env_file": wholePath(func(p string, args map[string]any) (any, error) {
		required, err := optStrListArg(args, "required_keys")
		if err != nil {
			return nil, err
		}
		return storage.CheckEnvFile(p, required), nil
	}),

	// system
	"system.read_uptime": pathFS(func(fsys fs.FS, name string, _ map[string]any) (any, error) {
		v, err := system.ReadUptime(fsys, name)
		if err != nil {
			return nil, systemErr(err)
		}
		return v, nil
	}),
	"system.read_loadavg": pathFS(func(fsys fs.FS, name string, _ map[string]any) (any, error) {
		v, err := system.ReadLoadavg(fsys, name)
		if err != nil {
			return nil, systemErr(err)
		}
		return v, nil
	}),
	"system.read_meminfo": pathFS(func(fsys fs.FS, name string, _ map[string]any) (any, error) {
		v, err := system.ReadMeminfo(fsys, name)
		if err != nil {
			return nil, systemErr(err)
		}
		return v, nil
	}),
	"system.parse_notifier_text": text(func(t string, _ map[string]any) (any, error) {
		// Python returns None when the file holds no recognisable count. A pair
		// of zeroes would read as a host with nothing to install, which is the
		// same file's other meaning.
		if u, s, ok := system.ParseNotifierText(t); ok {
			return []int64{u, s}, nil
		}
		return nil, nil
	}),
	"system.parse_apt_upgradable": text(func(t string, _ map[string]any) (any, error) {
		u, s := system.ParseAptUpgradable(t)
		return []int64{u, s}, nil
	}),

	// updater
	"updater.parse_sha256_line": text(func(t string, args map[string]any) (any, error) {
		asset, err := strArg(args, "asset_name")
		if err != nil {
			return nil, err
		}
		// Python returns None when no line names the asset. An empty string would
		// be a digest the caller then fails to match, which is the same refusal
		// wearing the wrong reason.
		if v, ok := updater.ParseSHA256Line(t, asset); ok {
			return v, nil
		}
		return nil, nil
	}),
}

// systemErr names the CPython exception v1 raises for a failure the system
// readers report as an error.
//
// These are the first ported functions that fail rather than returning a
// no-answer value, so they are also the first to need the mapping. It is
// exhaustive on purpose: an unrecognised error falls through unwrapped and is
// recorded as GoError, which is not a code Python can produce and so cannot be
// mistaken for a reproduced behaviour.
func systemErr(err error) error {
	switch {
	case errors.Is(err, system.ErrShortFile):
		return pyError{Code: "IndexError"}
	case errors.Is(err, system.ErrBadNumber):
		return pyError{Code: "ValueError"}
	case errors.Is(err, fs.ErrNotExist):
		return pyError{Code: "FileNotFoundError"}
	case errors.Is(err, fs.ErrPermission):
		return pyError{Code: "PermissionError"}
	default:
		return err
	}
}

// traceArgs reads the two arguments both trace parsers take.
func traceArgs(args map[string]any, t string, parse func(string, string, int) network.TraceResult) (any, error) {
	target, err := strArg(args, "target")
	if err != nil {
		return nil, err
	}
	maxHops, err := intArg(args, "max_hops")
	if err != nil {
		return nil, err
	}
	return parse(target, t, maxHops), nil
}

// strArg reads a manifest argument the adapter requires.
//
// A missing or mistyped argument is this table and py_oracle's disagreeing about
// a case's signature, not a parser result, so it surfaces as an error on the case
// rather than being passed along as an empty string that reads like a target
// nobody set.
func strArg(args map[string]any, key string) (string, error) {
	v, ok := args[key]
	if !ok {
		return "", fmt.Errorf("manifest case has no %q argument", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("manifest argument %q is %T, want a string", key, v)
	}
	return s, nil
}

// optStrListArg reads a manifest argument the Python side reads with a .get(), so
// an absent one is a legitimate None rather than a signature disagreement.
//
// Absent becomes a nil slice, which is what a Python None means to the functions
// that take one: nothing was asked for. A present-but-wrong argument is still an
// error, because that is the two tables drifting.
func optStrListArg(args map[string]any, key string) ([]string, error) {
	v, ok := args[key]
	if !ok || v == nil {
		return nil, nil
	}
	items, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("manifest argument %q is %T, want a list", key, v)
	}
	// Length-preserving rather than deduplicated or sorted: the order of this list
	// and its duplicates both survive into the result the comparison sees.
	out := make([]string, 0, len(items))
	for i, item := range items {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("manifest argument %q[%d] is %T, want a string", key, i, item)
		}
		out = append(out, s)
	}
	return out, nil
}

// intArg reads a whole-number manifest argument. JSON has one number type, so
// the decoded value is a float64 and a fractional one means the manifest says
// something the Python signature could not accept either.
func intArg(args map[string]any, key string) (int, error) {
	v, ok := args[key]
	if !ok {
		return 0, fmt.Errorf("manifest case has no %q argument", key)
	}
	f, ok := v.(float64)
	if !ok {
		return 0, fmt.Errorf("manifest argument %q is %T, want a number", key, v)
	}
	if f != math.Trunc(f) {
		return 0, fmt.Errorf("manifest argument %q is %v, want a whole number", key, f)
	}
	return int(f), nil
}
