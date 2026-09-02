// Package doctorcmd implements `fleetfix doctor`, the "why did it say that"
// front door.
//
// It answers one question, in as many sections as that takes: what is this
// build about to do on this host, and with which files. An operator reaches for
// it after a report said something surprising -- a threshold that is not the one
// they wrote, a docker check reporting unavailable on a host running containers,
// a Tier 2 check skipped on a machine where they are certain they have sudo.
//
// # Why it shares check's resolver
//
// This is M3's exit criterion and the reason internal/resolve exists. Doctor and
// `check --json` must agree about which container runtime and which config files
// are in play, and the only way to guarantee that is for there to be one answer
// rather than two that ought to match. Both front doors take a *resolve.Resolved
// they were handed; neither reads a file of its own. So every line printed here
// is the value the collectors graded by, not this package's second opinion --
// which is what makes "but doctor says the threshold is 85" an impossible
// support call rather than a likely one.
//
// # Text, not JSON
//
// The inverse of checkcmd, deliberately. `check --json` is for a cron job and a
// human reads it second; doctor is for a human at a prompt with a puzzle, and
// has no unattended consumer to keep a schema stable for. Printing it as text
// also lets each piece describe itself -- config layers, thresholds, the runtime
// reason -- in the words that piece already uses in the report.
//
// # Why it always exits zero
//
// Doctor describes; it does not grade. A missing traceroute, an absent
// thresholds.yml and no container runtime are all ordinary states of an ordinary
// host, and the checks that care already report unavailable for them. An exit
// code here would have to rank those, and any ranking would be wrong for
// somebody's fleet. `check --json --strict` is the gate for "did this host
// answer everything"; this is the explanation of why it did not.
package doctorcmd

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/resolve"
)

// Options is one invocation.
type Options struct {
	Stdout io.Writer

	// Version is printed at the top, so a pasted doctor output identifies the
	// build it came from without anyone having to ask.
	Version string

	// Registry holds the checks this build knows. Nil means an empty registry:
	// doctor still describes the host, and says the build ships no checks, which
	// is a fact worth printing rather than crashing over.
	Registry *check.Registry

	// Resolved is the host and its configuration, read once. Nil resolves the
	// live host -- which is correct for a caller with nothing else to share it
	// with, and wrong for the CLI, whose whole point is to hand the same one to
	// both front doors.
	Resolved *resolve.Resolved

	// LogDestination describes where diagnostics are going, which the logger
	// knows and this package cannot work out. Empty prints nothing: a caller
	// that has not configured logging has nothing to say about it.
	LogDestination string
}

// Run writes the report and returns the process exit code.
//
// A write failure is returned rather than printed, for checkcmd's reason: the
// one stream this function must not put a diagnostic on is the one it writes to.
func Run(ctx context.Context, opts Options) (int, error) {
	if opts.Registry == nil {
		opts.Registry = check.NewRegistry()
	}
	if opts.Resolved == nil {
		opts.Resolved = resolve.New(resolve.Options{})
	}

	var b strings.Builder
	write(ctx, &b, opts)

	if _, err := io.WriteString(opts.Stdout, b.String()); err != nil {
		return exitcode.Unknown, fmt.Errorf("writing the report failed: %w", err)
	}
	return exitcode.OK, nil
}

// write renders every section into b.
//
// Built in memory and written once, so a report that fails halfway through does
// not leave the operator reading a truncated host description and wondering
// which half is missing.
func write(ctx context.Context, b *strings.Builder, opts Options) {
	r := opts.Resolved

	fmt.Fprintf(b, "fleetfix %s\n", opts.Version)
	if opts.LogDestination != "" {
		fmt.Fprintf(b, "diagnostics: %s\n", opts.LogDestination)
	}

	section(b, "Host", []string{
		field("hostname", r.Host.Hostname),
		field("distro", r.Host.Distro),
		field("kernel", r.Host.Kernel),
		field("arch", r.Host.Arch),
		field("boot id", r.Host.BootID),
	})

	section(b, "Operator", []string{
		field("unix user", r.Operator.UnixUser),
		field("auth principal", r.Operator.AuthPrincipal),
		field("source ip", r.Operator.SourceIP),
	})

	// Opening the trail is the only way to learn whether it can be opened:
	// /var/log exists and is root-owned on every target distro, so a stat says
	// nothing and a permission check that raced the open would answer for a
	// moment that has passed. doctor is the one front door allowed to pay that
	// cost -- `check --json` runs from a scheduler and must leave nothing behind.
	auditLines := []string{}
	switch w, err := r.Audit(); {
	case err != nil:
		auditLines = append(
			auditLines,
			field("path", "unwritable"),
			field("error", err.Error()),
		)
	default:
		auditLines = append(auditLines, field("path", w.Path()))
		if reason := r.AuditFallback(); reason != "" {
			// Not a failure, and worth a line of its own: an operator who greps
			// /var/log and finds nothing needs to be told where the records went
			// and why, not left to infer it from a path they did not expect.
			auditLines = append(auditLines, field("fell back because", reason))
		}
	}
	section(b, "Audit trail", auditLines)

	// The privilege probe and the container probe both cost a subprocess, and
	// both are memoised on Resolved -- so asking here is free for a `check` that
	// already asked, and is the same answer either way. That is the point.
	priv := r.Privilege.State(ctx)
	root := "not root"
	if priv.IsRoot {
		root = "root"
	}
	tier2 := "unavailable"
	if priv.CanTier2 {
		tier2 = "available"
	}
	section(b, "Privilege", []string{
		field("uid", fmt.Sprintf("%d (%s)", priv.UID, root)),
		field("tier 2", tier2+" -- "+priv.Reason),
	})

	rt := r.Container(ctx)
	daemon := "the daemon did not answer"
	switch {
	case rt.Available:
		daemon = "the daemon answered"
	case !rt.Found():
		// Nothing was asked, so "did not answer" would be a lie about a probe
		// that never ran.
		daemon = "nothing to ask"
	}
	lines := []string{field("detected", rt.Reason), field("probe", daemon)}
	if rt.Endpoint != "" {
		lines = append(lines, field("endpoint", rt.Endpoint))
	}
	section(b, "Container runtime", lines)

	section(b, "Configuration", or(r.DescribeConfig(), "no configuration is read by this build"))
	section(b, "Configuration warnings", or(r.Warnings, "none"))
	section(b, "Thresholds", or(r.Thresholds.Describe(), "none"))
	section(b, "External programs", programs(opts.Registry, r))
	section(b, "Checks", checks(opts.Registry))
}

// programs is the inventory of external commands this build's checks shell out
// to, each with the checks that want it.
//
// From Spec.NeedsBins rather than from a list in this file, because the runner's
// presence gate reads the same field: a check whose binary is missing reports
// unavailable without running, and this section is that decision made visible
// before the run rather than explained after it. A hand-maintained list here
// would drift the first time a domain added a dependency, and would drift
// silently.
func programs(reg *check.Registry, r *resolve.Resolved) []string {
	wanted := map[string][]string{}
	for _, spec := range reg.Specs() {
		for _, bin := range spec.NeedsBins {
			wanted[bin] = append(wanted[bin], string(spec.ID))
		}
	}
	if len(wanted) == 0 {
		return []string{"  none: no check in this build shells out"}
	}

	names := make([]string, 0, len(wanted))
	for name := range wanted {
		names = append(names, name)
	}
	sort.Strings(names)

	// Widths from the content rather than from a guess, because a guess is wrong
	// exactly once and then stays wrong: `systemd-analyze` is fifteen characters
	// and overflowed a twelve-wide column, which shunted every path on that row
	// out of alignment with the rest of the table.
	found := make([]string, len(names))
	nameCol, pathCol := 0, 0
	for i, name := range names {
		found[i] = "not installed"
		if path, err := r.Looker.Look(name); err == nil {
			found[i] = path
		}
		nameCol = max(nameCol, len(name))
		pathCol = max(pathCol, len(found[i]))
	}

	lines := make([]string, 0, len(names))
	for i, name := range names {
		// The wanting checks come out of Specs, which is already id-ordered, so
		// this list is stable without sorting it again.
		lines = append(lines, fmt.Sprintf("  %-*s  %-*s  (%s)",
			nameCol, name, pathCol, found[i], strings.Join(wanted[name], ", ")))
	}
	return lines
}

// checks counts what this build ships and how much of it a bare `fleetfix check`
// would run.
//
// The gap between the two numbers is the line worth printing: a check that is
// registered and out of the default set looks exactly like a missing check to an
// operator grepping a report for it.
func checks(reg *check.Registry) []string {
	specs := reg.Specs()
	if len(specs) == 0 {
		return []string{"  none: this build ships no checks"}
	}

	byDefault := 0
	var withheld []string
	for _, spec := range specs {
		if spec.InDefault {
			byDefault++
			continue
		}
		withheld = append(withheld, string(spec.ID))
	}

	lines := []string{
		field("registered", fmt.Sprintf("%d in %d domains", len(specs), len(reg.Domains()))),
		field("default run", fmt.Sprintf("%d", byDefault)),
	}
	if len(withheld) > 0 {
		lines = append(lines, field("only when named", strings.Join(withheld, ", ")))
	}
	return lines
}

// section writes a heading and its lines, or nothing at all when there are none.
func section(b *strings.Builder, title string, lines []string) {
	if len(lines) == 0 {
		return
	}
	fmt.Fprintf(b, "\n%s\n", title)
	for _, line := range lines {
		fmt.Fprintln(b, line)
	}
}

// field renders one labelled value. An empty value is printed as "(none)"
// rather than omitted: a blank auth principal and a missing one read the same
// to a person, and only one of them is worth a support question.
func field(label, value string) string {
	if value == "" {
		value = "(none)"
	}
	return fmt.Sprintf("  %-15s %s", label, value)
}

// or renders a list, or one line saying it was empty. Indented to match field,
// so a section does not change shape depending on whether it found anything.
func or(lines []string, empty string) []string {
	if len(lines) == 0 {
		return []string{"  " + empty}
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		out = append(out, "  "+line)
	}
	return out
}
