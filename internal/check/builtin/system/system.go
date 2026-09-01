// Package system registers the system domain's checks.
//
// The five facts v1's dashboard led with -- load, memory, temperature, pending
// updates, uptime -- as five selectable checks. The parsing is
// internal/core/system, ported byte-for-byte; the bounds are internal/threshold,
// which holds four of its seven rules for this domain alone. What is left here is
// deciding what a reading means, and saying so in a form the TUI, `check --json`
// and the agent can each use without reimplementing it.
//
// # Why these are five checks and not one
//
// v1 rendered them as one pane and graded them with four separate severity
// functions, which is the shape of five checks wearing one coat. Split, an
// operator can select the one they care about -- `--check system.memory` on a box
// that runs hot by design -- and each carries its own budget, its own status and
// its own reason for having nothing to say. A host with no thermal sensors is
// unavailable for temperature and fine for everything else, and one pane could
// only be both at once.
//
// # Reads, not subprocesses
//
// Four of the five are file reads under /proc and /sys, through hostfs, so a test
// drives them with an fstest.MapFS and they cost nothing on a busy host. Only
// system.updates may shell out, and only after the cheap answer -- the MOTD
// fragment update-notifier already wrote -- turns out not to be there.
package system

import (
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
)

// The system domain's check ids. Public API: they appear in checks[], in --check
// selectors, and in whatever Ansible an operator writes against them.
const (
	LoadID    check.ID = "system.load"
	MemoryID  check.ID = "system.memory"
	ThermalID check.ID = "system.thermal"
	UpdatesID check.ID = "system.updates"
	UptimeID  check.ID = "system.uptime"
)

// Metric names, likewise public: these are what a dashboard and --prom pin on.
//
// The four that a rule grades are spelled exactly as the rule is -- cpu.load_per_cpu,
// mem.used_pct, thermal.temp_c, updates.security -- so a dashboard panel and the
// bound it draws a line at are named the same thing. The rest are the context an
// operator needs to act on that number and are named alongside it.
const (
	LoadPerCPUMetric = "cpu.load_per_cpu"
	Load1Metric      = "cpu.load1"
	Load5Metric      = "cpu.load5"
	Load15Metric     = "cpu.load15"
	CPUCountMetric   = "cpu.count"

	MemUsedPctMetric   = "mem.used_pct"
	MemTotalMetric     = "mem.total_bytes"
	MemAvailableMetric = "mem.available_bytes"
	MemUsedMetric      = "mem.used_bytes"
	SwapUsedPctMetric  = "swap.used_pct"
	SwapTotalMetric    = "swap.total_bytes"
	SwapUsedMetric     = "swap.used_bytes"

	TempMetric = "thermal.temp_c"

	SecurityUpdatesMetric = "updates.security"
	UpgradableMetric      = "updates.upgradable"

	UptimeMetric = "system.uptime_seconds"
)

// DefaultNotifierPath is the MOTD fragment update-notifier writes.
//
// A plain path rather than something under a hostfs root, for the reason
// netprobe's resolv.conf is: hostfs roots are pseudo-filesystems the kernel
// writes, and this is an ordinary file written by a package's cron job.
const DefaultNotifierPath = "/var/lib/update-notifier/updates-available"

// A Source is where this domain reads from. The zero value is unusable; New
// builds the live one and a test builds one with whichever seams it needs.
//
// A struct of seams rather than an argument list, for netprobe's reason: there
// are five, a test typically stages one, and the fields are exported so it can
// set that one and leave the rest alone.
type Source struct {
	// Host is /proc and /sys: loadavg, meminfo, uptime, the thermal zones.
	Host hostfs.Host

	// Run is the subprocess seam, for the one question that needs one.
	Run cmdrun.Runner

	// CPUs is the divisor the load rule grades against.
	CPUs func() int

	// NotifierPath is the update-notifier fragment. A field rather than the
	// constant so a test drives a captured file.
	NotifierPath string

	// ReadFile reads NotifierPath. A seam of its own because that file sits
	// outside both hostfs roots, and giving it a third root would imply it is a
	// pseudo-filesystem too.
	ReadFile func(name string) (string, error)
}

// New is the live host.
func New() Source {
	return Source{
		Host:         hostfs.New(),
		Run:          cmdrun.New(),
		CPUs:         numCPU,
		NotifierPath: DefaultNotifierPath,
		ReadFile:     readFile,
	}
}

// readBudget covers one read of a /proc or /sys file. Generous for a page the
// kernel renders on open, and it is not zero because a read of a pseudo-file can
// block on the driver behind it -- a wedged thermal sensor is the routine case.
const readBudget = 2 * time.Second

// readFile is the live plain-path reader, for the one file this domain reads that
// is not under a hostfs root.
//
// v1 decodes it with errors="replace". Not reproduced literally, and the reason it
// does not matter: Go's regexp decodes an invalid byte as U+FFFD while matching,
// which is the same substitution, and neither the digits nor the words these
// patterns look for can be spelled with one.
func readFile(name string) (string, error) {
	b, err := os.ReadFile(name) //nolint:gosec // the path is a field with a fixed default, not operator input
	return string(b), err
}

// numCPU is the load rule's divisor, and it is worth naming what it counts.
//
// runtime.NumCPU is affinity-aware where v1's os.cpu_count() is not, so a
// fleetfix pinned to two cores of a sixteen-core host divides by two here and by
// sixteen in v1. Affinity-aware is the more useful of the two -- the load this
// process shares a runqueue with is the load on the CPUs it can actually run on
// -- and the divergence only appears under a cpuset, which is a deliberate act.
func numCPU() int { return runtime.NumCPU() }

// Checks returns the system domain's checks, wired to one source.
//
// A constructor rather than registration by init(), for the reason builtin.Checks
// gives: what a build runs must not depend on what the linker kept.
func Checks(src Source) []check.Check {
	return []check.Check{
		load{src: src},
		memory{src: src},
		thermal{src: src},
		updates{src: src},
		uptime{src: src},
	}
}

// cpuCount is the divisor, floored at one.
//
// v1's max(cpu_count, 1) guards os.cpu_count() returning None; here it guards a
// staged seam and a runtime that has never returned zero. Kept because dividing
// the load by zero would report every host as +Inf per CPU, which grades crit and
// is the loudest possible way to be wrong.
func (s Source) cpuCount() int {
	if s.CPUs == nil {
		return 1
	}
	return max(s.CPUs(), 1)
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

// plain is a metric with nothing to label it by. Most of this domain's readings
// are one number about the whole host -- there is one memory and one load -- and
// an empty map is the honest shape rather than inventing a dimension.
func plain(name string, value float64, unit, help string) check.Metric {
	return gauge(name, value, unit, map[string]string{}, help)
}

// unreadable is what a check reports when the file it lives on will not read.
//
// error rather than unavailable, and the distinction is the whole reason this
// helper is separate from an absence: every path this domain reads is one the
// kernel maintains on every Linux host, so a failure to read it is a fault --
// a namespace mounted without /proc, a seccomp filter, a full inode table -- and
// not a host that simply does not have the thing.
func unreadable(what string, err error) check.Result {
	return check.Result{
		Status:  check.StatusError,
		Summary: what + " could not be read",
		Error:   err.Error(),
	}
}

// ungraded is the answer when the grading policy has no rule for what this check
// measures.
//
// error, not ok, for the reason the disk domain gives: threshold.Merge guarantees
// every shipped rule survives whatever an operator wrote, so arriving here means a
// Runner was wired with a policy that is not the host's -- and a check that read
// 99% and called it fine because nothing graded it is the permanently-green
// failure this layer exists to prevent. Reported before the read, since the answer
// cannot depend on the host.
func ungraded(rule, what string) check.Result {
	return check.Result{
		Status:  check.StatusError,
		Summary: "nothing graded this host's " + what,
		Error:   fmt.Sprintf("the grading policy has no %s rule", rule),
	}
}

// kbToBytes converts meminfo's units. The kernel writes "kB" and means KiB, which
// is why the multiplier is 1024 and not 1000.
func kbToBytes(kb int64) float64 { return float64(kb) * 1024 }

// humanBytes renders a byte count for a summary line. Binary steps, matching
// what the kernel reported in, and one decimal past the first unit.
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

// plural counts a noun for a summary line. int64 because the widest thing this
// domain counts is a package total, and narrowing at the two call sites that
// count CPUs and sensors is cheaper than keeping a second helper in step.
func plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// firstLine is one line of a tool's complaint. A summary field carrying apt's
// entire stderr is a field nobody reads.
func firstLine(s, fallback string) string {
	for _, line := range strings.Split(s, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
	}
	return fallback
}
