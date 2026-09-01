// Package storage registers the operator-driven storage checks.
//
// Two questions, and neither one has a default answer: what is rotting in this
// directory, and is this dotenv file sound. v1 asked both from
// screens/storage.py, where the directory came from a text box the operator
// typed into and the dotenv path came from another one.
//
// # Why both are operator-driven
//
// A path is the one argument nobody can guess, which is what Spec.Params is for
// -- and check.Spec says so in as many words: "Inspecting a path is the example
// -- there is no sensible default path." v1 defaulted the scan root to the
// inspect target's home and fell back to the invoking user's, which on a fleet
// host means root's home: a directory that is empty on most machines and, on the
// ones where it is not, holds nothing an operator wanted a report about. So both
// checks are InDefault: false and take their path as a parameter. A bare
// `fleetfix check` skips them and says it skipped them; `--check storage.stale
// --param root=/home/alice` runs one.
//
// That also means the seam here is a real directory rather than an fs.FS. The
// other domains stage a synthetic /proc or /var/log because their input is fixed
// and unreachable from a test; this one is pointed at a path an operator named,
// and the path it was given is part of the answer it returns. A tempdir is the
// honest seam for it, which is the reasoning internal/core/storage.CheckEnvFile
// already gives for taking a string.
//
// # Why the domain grades boolean, or not at all
//
// internal/threshold has no storage rule, so trips[] is empty for both checks.
// storage.stale grades nothing at all, for logsqueeze's reason: four gigabytes of
// old dumps is unremarkable on a host with a terabyte free and urgent on one with
// two gigabytes, and disk.usage already grades that. storage.env grades boolean,
// for the docker domain's reason: a missing file, an unparseable line and an
// absent required key are categorical facts, not numbers compared to a bound.
//
// # Why no value ever leaves this package
//
// A dotenv file holds credentials. internal/core/storage.CheckEnvFile returns the
// parsed values because it is a parser and its differential twin returns them
// too; this check reports key *names* only, and drops the raw line from every
// issue it forwards. A report is written to disk, shipped to a collector and
// pasted into tickets, and "not in KEY=value form" is the whole of what an
// operator needs to go and look at line 14 themselves.
package storage

import (
	"fmt"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
)

// The storage domain's check ids. Public API: they appear in checks[], in
// --check selectors, and in whatever Ansible an operator writes against them.
const (
	StaleID check.ID = "storage.stale"
	EnvID   check.ID = "storage.env"
)

// Metric names, likewise public.
//
// Named for their subject rather than the domain, which is the convention the
// system domain set with cpu.* and mem.*: what one set counts is stale files and
// what the other counts is a dotenv file, and "storage" is the name of the nav
// pane they happen to share.
const (
	StaleBytesMetric   = "stale.bytes"
	StaleFilesMetric   = "stale.files"
	StaleLargestMetric = "stale.largest_bytes"

	DotenvKeysMetric    = "dotenv.keys"
	DotenvIssuesMetric  = "dotenv.issues"
	DotenvMissingMetric = "dotenv.missing_keys"
)

// Parameter names, public for the same reason the ids are: an operator types
// them after --param.
const (
	RootParam          = "root"
	OlderThanDaysParam = "older_than_days"
	PathParam          = "path"
	RequiredKeysParam  = "required_keys"
)

// DefaultStaleAgeDays is v1's DEFAULT_STALE_AGE_DAYS
// (modules/storage/stale.py:18). A month is long enough that a file still in use
// has been touched since, and short enough that a debugging dump from the last
// incident shows up.
const DefaultStaleAgeDays = 30

// staleBudget caps the walk.
//
// Three times logsqueeze's, and for a reason that is not timidity: /var/log is a
// directory, and this walks whatever the operator named -- a home, a data volume,
// conceivably /. The prune list is what keeps that bounded in the ordinary case;
// the budget is what keeps it finite in the case the prune list did not
// anticipate. v1 had no timeout at all, which is fine for a button someone
// pressed and wrong for a check a cron job runs.
const staleBudget = 60 * time.Second

// envBudget caps the dotenv read. One stat and one read of a file that is
// measured in lines, so this is a backstop against a path that turns out to be a
// fifo rather than a bound anyone should reach.
const envBudget = 5 * time.Second

// stepCap bounds how many findings get a line of their own. The remainder is
// stated rather than dropped quietly, and data[] carries everything regardless.
// Ten is what the services and logsqueeze domains settled on.
const stepCap = 10

// Checks returns the domain's checks.
//
// Takes nothing, alone among the domains. There is no subprocess to route
// through a runner, no synthetic filesystem to stage and no config to thread:
// both checks are handed their path by the operator at run time and read it with
// syscalls. The one seam either of them has is the clock the age cutoff is
// measured against, which only this package's own tests need and which they set
// on the struct directly.
//
// A constructor rather than registration by init(), for the reason builtin.Checks
// gives: what a build runs must not depend on what the linker kept.
func Checks() []check.Check {
	return []check.Check{stale{}, env{}}
}

func gauge(name string, value float64, unit string, labels map[string]string, help string) check.Metric {
	return check.Metric{
		Name:   name,
		Value:  value,
		Unit:   unit,
		Labels: labels,
		Kind:   check.Gauge,
		Help:   help,
	}
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// humanBytes renders a byte count for a summary line. Binary steps, one decimal
// past the first unit -- the same rendering the docker, system and logsqueeze
// domains use, so two lines of one report do not disagree about what a gigabyte
// is.
func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB", "PB"}
	size := float64(n)
	i := 0
	for ; size >= 1024 && i < len(units)-1; i++ {
		size /= 1024
	}
	if i == 0 {
		return fmt.Sprintf("%d B", int64(size))
	}
	return fmt.Sprintf("%.1f %s", size, units[i])
}
