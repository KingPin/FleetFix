// Package updatecmd implements `fleetfix update`.
//
// # Why the default does not install
//
// A bare `fleetfix update` reports what is available and installs nothing.
// Installing takes --apply. v1 lit a banner and waited for the operator to press a
// key; there is no key to press in a pipeline, and the two obvious replacements are
// both wrong for a fleet tool. Prompting on stdin hangs an unattended run, or --
// worse -- reads whatever the next line of a script happened to be. Installing by
// default makes `fleetfix update` in a cron job a self-replacing binary, which is
// the one thing the updater's package doc says it must not be.
//
// So the confirmation is the flag. Typing --apply is a deliberate act by someone
// who read the line above it, which is what the keypress was for.
//
// # Why it exits zero when an update is available
//
// The exit code answers "did the command do what was asked", not "is this host up
// to date". A report-only run that found a release did its job. Making
// availability a non-zero code would put a routine, expected state into the same
// channel as "GitHub would not answer", and every wrapper would have to tell them
// apart by parsing the text anyway.
package updatecmd

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/KingPin/FleetFix/v2/internal/exitcode"
	"github.com/KingPin/FleetFix/v2/internal/resolve"
	"github.com/KingPin/FleetFix/v2/internal/updater"
)

// Options is one invocation.
type Options struct {
	Stdout io.Writer

	// Version is the running build, which is what a release is compared against.
	Version string

	// Resolved supplies the cache path and the audit writer. Nil resolves the live
	// host, which is right for a caller with nothing to share one with.
	Resolved *resolve.Resolved

	// Checker asks what the latest release is. Nil builds one over Resolved's
	// paths, with the cache write kept and every read treated as stale -- an
	// operator who typed the command is owed a fresh answer, and the file is still
	// worth leaving behind for the next launch banner.
	Checker *updater.Checker

	// Installer downloads and swaps. Nil builds one for the checker's asset, so
	// the binary that is verified is the one that was asked for.
	Installer *updater.Installer

	// Apply installs. False reports and stops.
	Apply bool
}

// Run reports the available release and, with Apply, installs it.
//
// The returned error is the caller's to print on stderr. Nothing diagnostic goes to
// Stdout: an operator piping this into a log wants the outcome, and a wrapper that
// greps for "installed" should not also match the reason it was not.
func Run(ctx context.Context, opts Options) (int, error) {
	host := opts.Resolved
	if host == nil {
		host = resolve.New(resolve.Options{})
	}

	check := opts.Checker
	if check == nil {
		check = freshChecker(host)
	}

	rel, available, err := check.Check(ctx, opts.Version)
	if err != nil {
		return exitcode.Unknown, err
	}
	if !available {
		// Also the answer on an architecture this project publishes no binary for.
		// Saying "no newer release" there would be a lie by omission, but saying so
		// needs a fact the checker deliberately does not report, and the operator on
		// such a host built it themselves.
		return exitcode.OK, say(opts.Stdout, "fleetfix %s is the latest release for this host.\n", opts.Version)
	}

	if !opts.Apply {
		return exitcode.OK, describe(opts.Stdout, opts.Version, rel)
	}
	return apply(ctx, opts, host, check.AssetName, rel)
}

// freshChecker is the checker `update` builds when it was handed none.
//
// A zero TTL rather than no cache path: every read is stale, so the answer is
// always the current one, and the file is still written for the next launch
// banner to find. A command an operator typed should not be answered from fifty
// minutes ago, and should not cost the banner its cache either.
func freshChecker(host *resolve.Resolved) *updater.Checker {
	c := updater.NewChecker(host.Paths)
	c.CacheTTL = 0
	return c
}

// describe is the report-only output: what is out, and how to take it.
func describe(w io.Writer, current string, rel updater.Release) error {
	if err := say(w, "fleetfix %s is available; this host runs %s.\n", rel.Version, current); err != nil {
		return err
	}
	if rel.HTMLURL != "" {
		if err := say(w, "Release notes: %s\n", rel.HTMLURL); err != nil {
			return err
		}
	}
	return say(w, "Run `fleetfix update --apply` to install it.\n")
}

// apply installs the release, refusing before the download when it can.
func apply(ctx context.Context, opts Options, host *resolve.Resolved, assetName string, rel updater.Release) (int, error) {
	install := opts.Installer
	if install == nil {
		// The checker's asset name, not a second lookup: the digest is checked
		// against the name that was downloaded, and two derivations of one fact
		// can disagree in a way that verifies the wrong file.
		install = updater.NewInstaller(assetName)
	}
	if install.Target == "" {
		// Resolved here rather than left to Apply, so the writability check below
		// and the swap that follows it are asking about the same path. Working it
		// out twice would let the check pass for one binary and the install fail
		// for another, which is a pre-check that is worse than not having one.
		install.Target, _ = updater.InstallTarget()
	}

	// Checked before the download, not after: an operator on a host where this
	// cannot land should be told so in a second rather than after a binary has
	// crossed the network. It is not a guarantee -- sudo may be on PATH with no
	// cached credential -- and the swap reports that case in sudo's own words.
	if !updater.HaveWritableTarget(install.Target, host.Looker) {
		return exitcode.Unknown, fmt.Errorf(
			"%s is not writable and sudo is not available; install the release from %s by hand",
			install.Target, rel.HTMLURL,
		)
	}

	// The trail before the action, and a refusal if there is none. This is the one
	// place in the CLI that changes the host, and an unrecorded binary swap is how
	// a fleet ends up with a version nobody can account for. The writer is opened
	// here rather than inside Apply so the refusal happens before the download.
	trail, err := host.Audit()
	if err != nil {
		return exitcode.Unknown, fmt.Errorf("refusing to install with no audit trail: %w", err)
	}

	res, err := install.Apply(ctx, trail, rel)
	if err != nil {
		if errors.Is(err, updater.ErrDigestMismatch) {
			// Worth naming rather than passing through. Every other failure is a
			// bad day; this one means what arrived is not what was published.
			return exitcode.Unknown, fmt.Errorf("refusing to install an unverified binary: %w", err)
		}
		return exitcode.Unknown, err
	}

	if err := say(opts.Stdout, "Installed fleetfix %s to %s.\n", res.Version, res.Target); err != nil {
		return exitcode.Unknown, err
	}
	// No relaunch, deliberately: the operator may be mid-triage in another window,
	// and replacing the process they are working in is not this command's call.
	return exitcode.OK, say(opts.Stdout, "Restart fleetfix to use it.\n")
}

func say(w io.Writer, format string, args ...any) error {
	_, err := fmt.Fprintf(w, format, args...)
	return err
}
