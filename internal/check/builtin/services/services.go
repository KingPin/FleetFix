// Package services registers the systemd domain's checks.
//
// Two questions v1 asked of systemd: which units are failed right now, and what
// made the last boot slow. The parsing is internal/core/services, ported
// byte-for-byte; internal/threshold holds one rule for this domain, and it
// belongs to the first of the two.
//
// # Why the second one grades nothing
//
// There is no boot rule, and inventing one here would be picking a number in the
// wrong place -- the bounds live in internal/threshold so an operator can
// override them per host, and a bound that exists only in this file cannot be
// overridden at all. A slow boot is also not a fault in the way a failed unit is:
// the host is up, and the number is history until the next reboot. So
// services.boot reports and narrates, and its status is ok whenever systemd
// answered.
//
// OutlierMS is therefore a display threshold and not a grading one -- it decides
// which units are worth a line in steps[], exactly as v1 used it to decide which
// rows to highlight.
package services

import (
	"fmt"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

// The services domain's check ids. Public API: they appear in checks[], in
// --check selectors, and in whatever Ansible an operator writes against them.
const (
	FailedID check.ID = "services.failed"
	BootID   check.ID = "services.boot"
)

// Metric names, likewise public.
//
// FailedMetric is spelled exactly as the rule that grades it, so a dashboard
// panel and the bound it draws a line at are named the same thing. The two boot
// metrics are named for their subject rather than the domain, which is the
// convention the system domain set with cpu.* and mem.*.
const (
	FailedMetric    = "services.failed"
	SlowestMetric   = "boot.slowest_ms"
	SlowUnitsMetric = "boot.slow_units"
)

// OutlierMS is v1's OUTLIER_MS (modules/services/boot.py:19): a unit that took
// five seconds or more to start is worth pointing at.
//
// Exported for the reason docker's RestartLoopThreshold is: internal/threshold
// has no services.boot entry to look it up in, and an operator reading a line
// about a slow unit has to be able to find out what this build calls slow.
const OutlierMS int64 = 5_000

// The two subprocess budgets. v1 gave each of its calls a 10s timeout; the failed
// check may make two of them, so it gets both plus slack, and boot makes one.
const (
	failedBudget = 25 * time.Second
	bootBudget   = 15 * time.Second
)

// Checks returns the services domain's checks, wired to one runner.
//
// A constructor rather than registration by init(), for the reason builtin.Checks
// gives: what a build runs must not depend on what the linker kept.
func Checks(run cmdrun.Runner) []check.Check {
	return []check.Check{
		failed{run: run},
		boot{run: run},
	}
}

// notManagedBySystemd is the answer when the binary is not there.
//
// The runner's NeedsBins gate normally catches this first and writes the same
// verdict once. This is the path for a Runner with no Looker -- a test, and any
// front door that decided not to gate -- and it has to agree with the gate rather
// than reporting a fault, because a host without systemd is a fact about the
// host: an Alpine container, a BSD, an initramfs.
func notManagedBySystemd(bin string) check.Result {
	return check.Result{
		Status:  check.StatusUnavailable,
		Summary: bin + " is not installed on this host",
	}
}

// ungraded is the answer when the grading policy has no rule for what this check
// measures.
//
// error, not ok, for the reason the disk and system domains give: threshold.Merge
// guarantees every shipped rule survives whatever an operator wrote, so arriving
// here means a Runner was wired with a policy that is not the host's -- and a
// check that found four dead units and called the host fine because nothing
// graded it is the permanently-green failure this layer exists to prevent.
// Reported before the read, since the answer cannot depend on the host.
func ungraded(rule, what string) check.Result {
	return check.Result{
		Status:  check.StatusError,
		Summary: "nothing graded this host's " + what,
		Error:   fmt.Sprintf("the grading policy has no %s rule", rule),
	}
}

func gauge(name string, value float64, unit, help string) check.Metric {
	return check.Metric{
		Name:   name,
		Value:  value,
		Unit:   unit,
		Labels: map[string]string{},
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

// firstLine is one line of a tool's complaint. A summary field carrying
// systemctl's entire stderr is a field nobody reads.
func firstLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}
