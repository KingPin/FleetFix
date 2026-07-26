package main

import (
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"

	"github.com/KingPin/FleetFix/v2/internal/core/disk"
	"github.com/KingPin/FleetFix/v2/internal/core/network"
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
