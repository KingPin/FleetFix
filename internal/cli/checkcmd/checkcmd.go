// Package checkcmd implements `fleetfix check`.
//
// At M1 there is nothing to check: no collectors exist yet, and the report
// envelope described in the plan lands with them. What exists here is the output
// discipline the real command has to inherit, exercised end to end so the shipping
// pipeline can be proven before the software is:
//
//	JSON on stdout, everything else on stderr, always.
//
// That rule is the whole reason a cron job can pipe this into jq. It is easier to
// establish while the payload is two lines than to retrofit once collectors are
// writing progress output.
package checkcmd

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/KingPin/FleetFix/v2/internal/exitcode"
)

// SchemaUnimplemented is the schema tag this build emits.
//
// Deliberately not "fleetfix.check/v1". A consumer pins on the schema string, so
// emitting the real one from a build that inspected nothing would let a parser
// accept a document missing the host, operator, privilege and counts blocks the
// contract promises. A value that will never be a real schema makes the refusal
// happen at the consumer's version check, which is where it belongs.
const SchemaUnimplemented = "fleetfix.check/unimplemented"

// Report is this build's stand-in for the check envelope.
//
// No omitempty on any field, matching the rule the real wire structs live under:
// an absent key and a null key are different documents to a consumer, and the
// difference is invisible in Go until someone's parser breaks.
type Report struct {
	Schema          string   `json:"schema"`
	FleetFixVersion string   `json:"fleetfix_version"`
	Status          string   `json:"status"`
	ExitCode        int      `json:"exit_code"`
	Checks          []string `json:"checks"`
	Notes           string   `json:"notes"`
}

// Run writes the report to stdout and returns the process exit code.
//
// The status is "unknown" and the code is 3, not "ok" and 0. Nothing about this
// host was examined, and a build that reports ok for a host it never looked at is
// how a monitored fleet goes green while a disk fills. Under Nagios semantics
// unknown is exactly the right answer to "what is the state?" when the answer is
// "we did not find out".
//
// A failed write is returned rather than reported, because the one stream that must
// not carry a diagnostic is the one this function writes to. The caller owns stderr.
func Run(stdout io.Writer, version string) (int, error) {
	rep := Report{
		Schema:          SchemaUnimplemented,
		FleetFixVersion: version,
		Status:          "unknown",
		ExitCode:        exitcode.Unknown,
		// An empty slice, never nil: nil marshals to null, and []-versus-null is one
		// of the four cosmetic-difference classes the differential harness exists to
		// keep out of the diff.
		Checks: []string{},
		Notes:  "no checks are registered in this build; the fleetfix.check/v1 envelope arrives with the collectors",
	}

	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rep); err != nil {
		return exitcode.Unknown, fmt.Errorf("writing the report failed: %w", err)
	}
	return exitcode.Unknown, nil
}

// Usage is the subcommand's help text, written to the caller's chosen stream.
func Usage(w io.Writer) {
	fmt.Fprint(w, `Usage: fleetfix check [flags]

Collect host health and write the result to stdout as JSON.

Flags:
  --json    Emit JSON (the only implemented mode in this build).

This build registers no checks and reports status "unknown" with exit code 3.
`)
}
