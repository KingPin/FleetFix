// Package checkcmd implements `fleetfix check`, the fleet front door.
//
// The output discipline is the contract, and it is absolute:
//
//	JSON on stdout, everything else on stderr, always -- including on
//	catastrophic failure.
//
// A cron job pipes this into jq. If a run that could not happen wrote a bare
// error line to stdout, that parser breaks, and a broken parser is how a real
// failure turns into a silence nobody investigates. So every path through this
// package emits one valid document of the published schema: a config file that
// would not parse, a --check selector that names nothing, a build with no
// collectors. The document says what went wrong; the exit code says it too.
//
// Everything about the host comes from internal/resolve, which read it once.
// This package decides what to run and how to print it, and nothing else -- so
// `doctor` can describe exactly the configuration these checks graded by.
package checkcmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/report"
	"github.com/KingPin/FleetFix/v2/internal/resolve"
)

// A Format is an output mode.
//
// A named type rather than two booleans, so "both --json and --ndjson" is a
// state the caller cannot construct and this package does not have to rank.
type Format string

const (
	// FormatJSON is one indented document. The default: a person reads this too.
	FormatJSON Format = "json"

	// FormatNDJSON is the envelope, then one compact object per check. For a
	// consumer that streams rather than buffers.
	FormatNDJSON Format = "ndjson"
)

// ListSchema identifies `check --list` output.
//
// Its own schema string, versioned separately from the report: the listing
// describes the build's capabilities and the report describes a host, and a
// consumer pinning on one should not be broken by a change to the other.
const ListSchema = "fleetfix.checklist/v1"

// Options is one invocation. Every field has a working default, so the zero
// value plus a stdout is a real run against the live host.
type Options struct {
	Stdout io.Writer

	// Version is what the envelope reports as fleetfix_version.
	Version string

	// Registry holds the checks this build knows. Nil means an empty registry,
	// which is a run that reports it found nothing to do rather than a crash.
	Registry *check.Registry

	// Resolved is the host and its configuration, read once. Nil resolves the
	// live host.
	Resolved *resolve.Resolved

	// Runner runs the checks. Nil builds one from Resolved, which is what wires
	// the privilege gate and the grading policy the report describes.
	Runner *check.Runner

	// Include and Exclude are --check and --exclude selectors: an exact id or a
	// whole domain. Empty Include means the default set.
	Include []string
	Exclude []string

	// Params are --param values for operator-driven checks.
	Params map[string]string

	Format Format

	// List prints what this build can check instead of checking anything.
	List bool

	// ExitZero always exits 0. For a pipeline where a non-zero code kills the
	// job and the JSON is the actual product.
	ExitZero bool

	// Strict makes "we did not find out" a failure: a run that found nothing
	// wrong but skipped something exits unknown rather than ok. For a fleet
	// where an unmonitored host is the thing worth knowing about.
	Strict bool

	// Now is the clock. Nil means time.Now; injected so a test can assert the
	// envelope's timestamp rather than merely that it parses.
	Now func() time.Time
}

func (o Options) now() time.Time {
	if o.Now == nil {
		return time.Now()
	}
	return o.Now()
}

// Run performs one invocation and returns the process exit code.
//
// A write failure is returned rather than printed, because the one stream this
// function must not put a diagnostic on is the one it writes to. The caller owns
// stderr.
func Run(ctx context.Context, opts Options) (int, error) {
	if opts.Registry == nil {
		opts.Registry = check.NewRegistry()
	}
	if opts.Resolved == nil {
		opts.Resolved = resolve.New(resolve.Options{})
	}
	if opts.Format == "" {
		opts.Format = FormatJSON
	}

	if opts.List {
		return list(opts)
	}

	started := opts.now()
	meta := opts.Resolved.Meta(ctx)
	meta.Version = opts.Version
	meta.GeneratedAt = started

	checks, err := opts.Registry.Select(opts.Include, opts.Exclude)
	switch {
	case err != nil:
		// A typo in an Ansible play. Loud, and still a valid document: a warning
		// here would run nothing, find nothing wrong and exit 0, which is a
		// permanently green host.
		return emit(opts, report.Failed(meta, err.Error()))
	case len(checks) == 0:
		return emit(opts, report.Failed(meta, noChecks(opts)))
	}

	results := runner(opts).Run(ctx, checks, opts.Params)
	meta.Duration = opts.now().Sub(started)
	return emit(opts, report.New(meta, results))
}

// runner wires the check runner to the host that was already resolved, so a
// check cannot be gated by a different privilege answer or graded by a different
// policy than the envelope reports.
func runner(opts Options) *check.Runner {
	if opts.Runner != nil {
		return opts.Runner
	}
	return &check.Runner{
		Look:       opts.Resolved.Looker,
		CanTier2:   opts.Resolved.Privilege.CanTier2,
		Thresholds: opts.Resolved.Thresholds,
	}
}

// noChecks explains an empty selection in the operator's terms. Three different
// situations produce it and only one of them is a mistake.
func noChecks(opts Options) string {
	switch {
	case opts.Registry.Len() == 0:
		return "no checks are registered in this build"
	case len(opts.Include) == 0:
		return "no checks run by default; name one with --check, or `--list` to see them all"
	default:
		return "every selected check was also excluded"
	}
}

// emit writes the document and returns the process exit code.
func emit(opts Options, rep report.Report) (int, error) {
	write := report.WriteJSON
	if opts.Format == FormatNDJSON {
		write = report.WriteNDJSON
	}
	if err := write(opts.Stdout, rep); err != nil {
		return exitcode.Unknown, err
	}
	return code(opts, rep), nil
}

// code turns the report's verdict into the process's exit code.
//
// The document's exit_code is left alone on purpose: it states what was found,
// which is a fact about the host, while these two flags state what this
// invocation should do about it, which is a fact about the caller. A consumer
// reading exit_code out of a stored report a week later wants the finding.
func code(opts Options, rep report.Report) int {
	out := rep.ExitCode
	if opts.Strict && out == exitcode.OK && notDetermined(rep.Counts) > 0 {
		out = exitcode.Unknown
	}
	if opts.ExitZero {
		out = exitcode.OK
	}
	return out
}

// notDetermined counts the checks that produced no verdict about the host.
// Skipped and unavailable are ordinarily exit 0 -- a host without docker is not
// a broken host -- which is exactly the reading --strict overrides.
func notDetermined(c check.Counts) int { return c.Skipped + c.Unavailable }

// A Listing is `check --list` output: what this build can check, before any of
// it has been run.
//
// Same wire rules as the report: no omitempty, no nil slices. A consumer
// generating an Ansible task list off this reads it exactly as it reads the
// report.
type Listing struct {
	Schema          string        `json:"schema"`
	FleetFixVersion string        `json:"fleetfix_version"`
	Checks          []ListedCheck `json:"checks"`
}

// A ListedCheck is one check's spec, as an operator needs to read it.
type ListedCheck struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Domain string `json:"domain"`
	// Tier2 means the check needs root or a working sudo -n; on a host where
	// this process has neither, it will report skipped.
	Tier2 bool `json:"tier2"`
	// NeedsBins are the external programs it shells out to. Absent ones make it
	// report unavailable rather than error.
	NeedsBins []string `json:"needs_bins"`
	// Params non-empty means operator-driven: it needs an argument nobody can
	// guess, so a default run does not include it.
	Params []ListedParam `json:"params"`
	// BudgetMS is the effective cap, with the runner's default already applied.
	// Reporting a literal 0 for "whatever the default is" would make the listing
	// say a check has no time to run.
	BudgetMS  int64 `json:"budget_ms"`
	InDefault bool  `json:"in_default"`
}

// A ListedParam is one argument an operator-driven check takes.
type ListedParam struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Required    bool   `json:"required"`
	Default     string `json:"default"`
}

// list writes the listing. Exit 0: being asked what the build can do is not a
// statement about the host, so there is no verdict to report.
func list(opts Options) (int, error) {
	out := Listing{
		Schema:          ListSchema,
		FleetFixVersion: opts.Version,
		Checks:          []ListedCheck{},
	}
	for _, spec := range opts.Registry.Specs() {
		out.Checks = append(out.Checks, listed(spec))
	}

	enc := json.NewEncoder(opts.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return exitcode.Unknown, fmt.Errorf("writing the listing failed: %w", err)
	}
	return exitcode.OK, nil
}

func listed(spec check.Spec) ListedCheck {
	budget := spec.Budget
	if budget <= 0 {
		budget = check.DefaultBudget
	}
	out := ListedCheck{
		ID:        string(spec.ID),
		Title:     spec.Title,
		Domain:    spec.Domain,
		Tier2:     spec.Tier2,
		NeedsBins: append([]string{}, spec.NeedsBins...),
		Params:    []ListedParam{},
		BudgetMS:  budget.Milliseconds(),
		InDefault: spec.InDefault,
	}
	for _, p := range spec.Params {
		out.Params = append(out.Params, ListedParam(p))
	}
	return out
}

// Usage is the subcommand's help text, written to the caller's chosen stream.
func Usage(w io.Writer) {
	fmt.Fprint(w, `Usage: fleetfix check [flags]

Collect host health and write the result to stdout as JSON. Everything else --
warnings, diagnostics, errors -- goes to stderr, so stdout is always a valid
document of the published schema, including when the run could not happen.

Flags:
  --json           Emit one indented JSON document (the default).
  --ndjson         Emit the envelope, then one compact object per check.
  --check SEL      Run only these; SEL is a check id or a whole domain.
                   Repeatable, and accepts a comma-separated list.
  --exclude SEL    Do not run these. Applied after --check.
  --param K=V      Supply an argument to an operator-driven check. Repeatable.
  --list           Print what this build can check and exit, without checking.
  --exit-zero      Always exit 0. The JSON still reports the real verdict.
  --strict         Exit unknown when a check was skipped or unavailable, rather
                   than treating "we did not find out" as fine.

Exit codes: 0 ok, 1 warn, 2 crit, 3 unknown.
`)
}
