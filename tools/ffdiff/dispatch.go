package main

import (
	"github.com/KingPin/FleetFix/v2/internal/core/disk"
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
}
