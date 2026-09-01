// Package docker registers the container domain's checks.
//
// Three checks over one resolved runtime: whether a runtime is here and answering,
// what its containers are doing, and what its storage is holding. The parsing is
// internal/core/docker, ported byte-for-byte; the runtime resolution is
// internal/container, resolved once per run. What is left here is the part v1 never
// had: deciding what a container's state means.
//
// # Why the domain grades boolean
//
// internal/threshold has no docker rule, so trips[] is empty for all three checks
// and the status comes from the fact itself. That is the honest shape rather than a
// gap: "45GB reclaimable" is not high or low without knowing the disk it sits on,
// and the three things this domain does judge -- a restart loop, a failing
// healthcheck, a container docker itself calls dead -- are the runtime's own
// verdicts, not a number compared to a bound. Inventing thresholds v1 never had, to
// grade quantities nobody set a policy for, would produce a domain that is warn on
// every busy host and tells an operator nothing.
//
// # Where the fault is named
//
// A dead daemon is reported crit by docker.runtime and unavailable by the other
// two. v1 caught FileNotFoundError at every call site, so a stopped daemon produced
// three separate confusing messages and no verdict at all; a domain that answered
// unavailable everywhere would be no better, because unavailable does not reach the
// exit code without --strict, and a host whose docker died would go green. One
// check owns the verdict, and the other two say why they have nothing to add.
package docker

import (
	"context"
	"fmt"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/container"
)

// The docker domain's check ids. Public API: they appear in checks[], in --check
// selectors, and in whatever Ansible an operator writes against them.
//
// Spelled "docker" rather than "container" because that is what v1's nav called it
// and what an operator will type. The ids outlive the day podman lands -- issue #7
// adds a dialect under these same three questions, not a fourth check.
const (
	RuntimeID    check.ID = "docker.runtime"
	ContainersID check.ID = "docker.containers"
	DiskID       check.ID = "docker.disk"
)

// Metric names, likewise public: these are what a dashboard and --prom pin on.
const (
	DaemonUpMetric     = "docker.daemon_up"
	ContainersMetric   = "docker.containers"
	RunningMetric      = "docker.containers_running"
	RestartLoopsMetric = "docker.restart_loops"
	RestartsMetric     = "docker.container_restarts"
	DiskBytesMetric    = "docker.disk_bytes"
	ReclaimableMetric  = "docker.reclaimable_bytes"
	ReclaimablePct     = "docker.reclaimable_pct"
)

// A Runtime answers which container runtime this host has and whether its daemon
// replies.
//
// A function rather than a resolved container.Runtime because the answer costs a
// subprocess, and a great many hosts in a fleet run no containers at all. The
// caller memoises -- resolve.Resolved.Container does -- so three checks asking is
// one `docker version`, and an invocation that selected no docker check never asks
// at all.
type Runtime func(context.Context) container.Runtime

// Checks returns the docker domain's checks, wired to one resolved runtime and one
// subprocess seam.
//
// A constructor rather than registration by init(), for the reason builtin.Checks
// gives: what a build runs must not depend on what the linker kept.
func Checks(run cmdrun.Runner, runtime Runtime) []check.Check {
	return []check.Check{
		runtimeCheck{runtime: runtime},
		containers{run: run, runtime: runtime},
		diskUsage{run: run, runtime: runtime},
	}
}

// unavailableBecause is what the two collectors report when there is no daemon to
// ask. The runtime's own Reason, not a sentence written here: docker's connection
// error names the socket it tried, which is the fact that resolves the call.
func unavailableBecause(rt container.Runtime) check.Result {
	return check.Result{Status: check.StatusUnavailable, Summary: rt.Reason}
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

func counter(name string, value float64, unit string, labels map[string]string, help string) check.Metric {
	m := gauge(name, value, unit, labels, help)
	m.Kind = check.Counter
	return m
}

func plural(n int, noun string) string { return pluralAs(n, noun, noun+"s") }

// pluralAs is plural for a noun whose plural is not the singular plus an s. Only
// "category" needs it today, and it is here rather than inline because a summary
// reading "4 categorys" is the kind of thing an operator screenshots.
func pluralAs(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// humanBytes is v1's _human_bytes (screens/docker.py:262), including its two
// quirks: the step is 1024 while the sizes docker printed were decimal, and bytes
// alone are whole numbers where every larger unit carries one decimal. Reproduced
// rather than corrected, because an operator comparing this line to v1's docker
// pane should not find two different numbers for one host.
func humanBytes(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
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
