package main

import (
	"fmt"
	"math"

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
