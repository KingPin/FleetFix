// Package report is the public JSON contract of `fleetfix check`.
//
// Everything in this file is a wire struct. Something outside this repository --
// a jq filter in a cron job, an Ansible task, a monitoring integration -- reads
// what these marshal to, so the field names and the document shape are an API,
// not an implementation detail. Three rules keep it one:
//
//	No omitempty, anywhere. An absent key and a null key are different documents
//	to a consumer, and the difference is invisible from inside Go until someone's
//	parser breaks on a host where the optional thing happened not to be there.
//
//	No nil slices. nil marshals to null and an empty slice marshals to [], and a
//	consumer that writes `.checks[]` gets an error on one and nothing on the
//	other. New fills them; the golden test proves it.
//
//	Schema is versioned separately from the binary. A consumer pins on Schema,
//	which changes only when a key is removed or repurposed. Adding a key is not a
//	break, so consumers must ignore ones they do not know.
//
// The status enum is frozen. Adding a seventh value is a breaking change, because
// every consumer that switches on it has an else branch that would silently start
// catching the new one.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
)

// Schema identifies the document shape. Consumers pin on this string.
//
// Deliberately not derived from the binary version: a 2.4.0 that added a check
// still emits fleetfix.check/v1, and a consumer that rejected it for the version
// mismatch would be rejecting a document it understands perfectly.
const Schema = "fleetfix.check/v1"

// A Host describes the machine the report is about.
//
// Enough to identify the host in an aggregation and to explain a result that
// depends on the platform -- a kernel-specific /proc layout, an arch-specific
// binary set -- and nothing more. This is a diagnostic document that leaves the
// host, so it carries what is needed to read it and not an inventory dump.
type Host struct {
	Hostname string `json:"hostname"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Kernel   string `json:"kernel"`
	// Distro is best-effort: the pretty name from os-release, empty when the
	// host does not have one. Empty rather than a guess.
	Distro string `json:"distro"`
	// BootID identifies this boot, so a consumer can tell "uptime reset" from
	// "the host was renamed". Empty where the kernel does not expose one.
	BootID string `json:"boot_id"`
}

// An Operator is who ran the check.
//
// Present in a read-only report for the same reason it is in the audit log: a
// `check --json` from a cron job and one from a person debugging are different
// events, and the difference is worth being able to see in an aggregation.
//
// AuthPrincipal is the vendor-neutral second-factor or SSO slot. FleetFix does
// not speak any auth protocol and never synthesizes a value here; it is empty
// unless something out of band put an identity in it.
type Operator struct {
	UnixUser      string `json:"unix_user"`
	AuthPrincipal string `json:"auth_principal"`
	SourceIP      string `json:"source_ip"`
}

// Privilege records what the process could do, which is why a check was skipped.
//
// Without it, a skipped Tier 2 check is indistinguishable from a check that was
// deselected, and an operator reading an aggregation cannot tell "this host is
// unmonitored" from "this host is monitored by an unprivileged agent".
type Privilege struct {
	UID      int  `json:"uid"`
	IsRoot   bool `json:"is_root"`
	CanTier2 bool `json:"can_tier2"`
	// Reason explains a false CanTier2 in the operator's terms -- a cached sudo
	// credential that expired reads very differently from a user not in sudoers.
	Reason string `json:"reason"`
}

// A Report is one `fleetfix check` run.
//
// Field order here is the key order on the wire, because encoding/json follows
// struct order for structs (it sorts only maps). Envelope first, verdict second,
// detail last: a human running this by hand sees the answer before the evidence,
// and a `head` of a large report is still useful.
type Report struct {
	Schema          string `json:"schema"`
	FleetFixVersion string `json:"fleetfix_version"`
	// GeneratedAt is RFC 3339 in UTC with millisecond precision, the same format
	// and the same truncation as the audit log's ts. One timestamp format across
	// the tool means one thing for a log pipeline to parse.
	GeneratedAt string `json:"generated_at"`
	DurationMS  int64  `json:"duration_ms"`

	Host      Host      `json:"host"`
	Operator  Operator  `json:"operator"`
	Privilege Privilege `json:"privilege"`

	Status   check.Status `json:"status"`
	ExitCode int          `json:"exit_code"`
	// Error explains a run that did not happen -- a config that would not parse,
	// a selector that named nothing. Empty on every run that reached the checks,
	// including one where individual checks errored; those explain themselves in
	// checks[].error, and conflating the two would make a single broken collector
	// look like a broken run.
	Error  string       `json:"error"`
	Counts check.Counts `json:"counts"`

	Checks []check.Result `json:"checks"`

	// ConfigWarnings is every complaint the config layers produced, from every
	// file, in the order they were found. In the report rather than on stderr
	// because a cron job discards stderr, and a threshold that was ignored for a
	// typo is exactly the thing that makes a fleet quietly stop alerting.
	ConfigWarnings []string `json:"config_warnings"`
}

// Meta is everything about a run that is not the checks themselves.
//
// A parameter object rather than eight arguments: every field is either a string
// or a small struct, and a positional call would be one transposition away from
// reporting the arch as the kernel with nothing to catch it.
type Meta struct {
	Version        string
	GeneratedAt    time.Time
	Duration       time.Duration
	Host           Host
	Operator       Operator
	Privilege      Privilege
	ConfigWarnings []string
}

// New assembles a report from a finished run.
//
// The status and the exit code are derived here rather than passed in, so there
// is exactly one place that decides what a set of results means. A caller that
// could pass its own would eventually pass one that disagrees with counts, and
// the disagreement would be between two keys of the same document.
func New(meta Meta, results []check.Result) Report {
	counts, status := check.Tally(results)

	out := Report{
		Schema:          Schema,
		FleetFixVersion: meta.Version,
		GeneratedAt:     Timestamp(meta.GeneratedAt),
		DurationMS:      meta.Duration.Milliseconds(),
		Host:            meta.Host,
		Operator:        meta.Operator,
		Privilege:       meta.Privilege,
		Status:          status,
		ExitCode:        status.ExitCode(),
		Counts:          counts,
		Checks:          make([]check.Result, 0, len(results)),
		ConfigWarnings:  meta.ConfigWarnings,
	}
	if out.ConfigWarnings == nil {
		out.ConfigWarnings = []string{}
	}
	for _, r := range results {
		// Normalize is idempotent and the runner already ran it. Running it again
		// covers the caller who assembled results some other way -- `doctor`, a
		// replay, a test -- because a nil slice reaching the wire is a consumer's
		// problem and not a caller's.
		out.Checks = append(out.Checks, r.Normalize())
	}
	check.SortResults(out.Checks)
	return out
}

// Failed is the report emitted when the run could not happen at all.
//
// A config file that would not parse, an unmatched --check selector, a registry
// with nothing in it. The rule is that stdout always carries a valid document of
// this schema, including on catastrophic failure: a cron job's parser breaking is
// how a real failure turns into a silence nobody investigates. Status is error
// and the exit code is 3, because "we did not find out" is not "everything is
// fine".
func Failed(meta Meta, reason string) Report {
	rep := New(meta, nil)
	rep.Status = check.StatusError
	rep.ExitCode = rep.Status.ExitCode()
	rep.Error = reason
	return rep
}

// Timestamp formats t the way every FleetFix record does: UTC, RFC 3339, three
// digits of subsecond, a literal Z.
//
// Truncated rather than rounded, matching v1, so a timestamp never names a
// millisecond the event had not reached yet. Ordering within a run comes from the
// audit log's seq, not from this.
func Timestamp(t time.Time) string {
	return t.UTC().Truncate(time.Millisecond).Format("2006-01-02T15:04:05.000Z")
}

// WriteJSON writes the report to w as indented JSON with a trailing newline.
//
// Indented because a person reads this too, and two spaces costs a few hundred
// bytes on a document that is already kilobytes. HTML escaping is off: Go escapes
// <, > and & by default, which would turn a perfectly ordinary path or a `>`
// in a process cmdline into > and make the document harder to read for no
// benefit outside a browser, which is not where this goes.
func WriteJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rep); err != nil {
		return fmt.Errorf("writing the report failed: %w", err)
	}
	return nil
}

// WriteNDJSON writes one compact JSON object per line: the envelope first with an
// empty checks array, then one object per check.
//
// For a consumer that streams -- a log shipper, a `while read` loop -- rather than
// buffering a whole document. The envelope goes first even though its status is
// only known at the end, because a reader that sees the host and the schema before
// the results can route the rest without buffering, which is the entire point.
func WriteNDJSON(w io.Writer, rep Report) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)

	envelope := rep
	envelope.Checks = []check.Result{}
	if err := enc.Encode(envelope); err != nil {
		return fmt.Errorf("writing the report envelope failed: %w", err)
	}
	for _, r := range rep.Checks {
		if err := enc.Encode(r); err != nil {
			return fmt.Errorf("writing check %s failed: %w", r.ID, err)
		}
	}
	return nil
}
