// Package builtin assembles the checks this build ships.
//
// One list, in one place, so "what does fleetfix check?" has an answer that does
// not depend on which packages a front door happened to import. Registration by
// init() would have given the opposite property: the set would be whatever the
// linker kept, it would differ between the TUI binary and a test binary, and a
// check could go missing from a release without anything failing.
//
// Each front door calls this and owns the registry it gets back. The registry is
// a value for the same reason -- a process-wide default makes a test that wants
// three checks fight whatever the rest of the binary registered.
package builtin

import (
	"context"
	"os"
	"sync"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/disk"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/docker"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/logsqueeze"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/network"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/services"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/storage"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/system"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/container"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// Deps is everything the collectors need from the host, in the shape that lets a
// test stage all of it.
//
// Every field is optional and every default is the live host, so a caller that
// only wants to know what this build can check -- `--list`, doctor's inventory,
// the registry test below -- supplies nothing and still gets the real set.
type Deps struct {
	// Run is the subprocess seam. Nil means the real one.
	Run cmdrun.Runner

	// Look answers "is this binary installed", which the network domain needs
	// before it can choose between traceroute and tracepath.
	Look cmdrun.Looker

	// Prober is the network domain's I/O seam: /proc, the resolver, the dialer
	// and the clock, alongside the two above. Nil means one built from Run and
	// Look over the live host, so the common case is to set those two and leave
	// this alone; a test that stages a whole fake machine sets this instead.
	Prober *netprobe.Prober

	// Probes is probes.yml, already resolved by internal/resolve. A pointer
	// because the zero Probes is a valid-looking value with no targets in it,
	// which would silently skip every network probe -- nil has to mean "nobody
	// said", and that means the defaults.
	Probes *corenet.Probes

	// Container answers which container runtime this host has and whether its
	// daemon replies. Nil means detect-and-probe over this Deps' own seams,
	// memoised for the run.
	//
	// The production caller passes resolve.Resolved.Container instead, so the
	// runtime doctor describes is the runtime the checks graded -- one probe per
	// invocation, shared, rather than one per front door.
	Container docker.Runtime

	// System is the system domain's read seams: /proc and /sys through hostfs, the
	// CPU count the load rule divides by, and the update-notifier fragment. Nil
	// means the live host wearing whichever Run the caller supplied, so a caller
	// with an ordinary machine underneath it sets nothing; a test that stages a
	// whole /proc sets this.
	//
	// A pointer for Probes' reason: the zero Source reads from a nil filesystem
	// and has no CPU count, which is not "the live host" but a Source that fails
	// every check it is handed to.
	System *system.Source

	// Logs is where the log-reclaim domain walks. Nil means the live /var/log at
	// v1's ten-mebibyte floor.
	//
	// A value would do here -- logsqueeze.Source is written so that its zero value
	// is the live default, precisely because a struct literal that forgot a field
	// must not turn into a walk reporting every empty rotated log on the host. It
	// is a pointer anyway, to match System and Probes: a reader of this struct
	// should not have to remember which of the seams treat their zero as "nobody
	// said" and which as "nothing".
	Logs *logsqueeze.Source

	// The storage domain has no field here, and that is not an omission. Both of
	// its checks are handed their path by the operator at run time and read it with
	// syscalls, so there is no subprocess to route, no synthetic filesystem to
	// stage and no config to thread -- the parameter is the seam, and it arrives
	// through Input rather than through this struct.
}

func (d Deps) runner() cmdrun.Runner {
	if d.Run == nil {
		return cmdrun.New()
	}
	return d.Run
}

// prober is the network seam, defaulted to the live host but wearing whichever
// of Run and Look the caller supplied, so one invocation's subprocesses all go
// through the seam it was resolved with.
func (d Deps) prober() *netprobe.Prober {
	if d.Prober != nil {
		return d.Prober
	}
	p := netprobe.New()
	if d.Run != nil {
		p.Run = d.Run
	}
	if d.Look != nil {
		p.Look = d.Look
	}
	return p
}

func (d Deps) looker() cmdrun.Looker {
	if d.Look == nil {
		return cmdrun.NewPATH()
	}
	return d.Look
}

func (d Deps) probes() corenet.Probes {
	if d.Probes == nil {
		return corenet.DefaultProbes()
	}
	return *d.Probes
}

// container is the fallback runtime resolver, for a caller that supplied no
// Container of its own -- a test, or a front door with nothing resolved yet.
//
// Both halves are inside the memo, so a run that selected no docker check costs
// neither the PATH lookup nor the `docker version`, and a run that selected all
// three costs one of each. That is the whole reason docker.Runtime is a function:
// most hosts in a fleet run no containers, and three probes per invocation on
// every one of them is a cost nobody would see and everybody would pay.
//
// The first caller's context bounds the probe and the answer stands for the run,
// which is resolve.Resolved.Container's rule too, and for its reason: a daemon
// that comes up mid-run must not make one check say unavailable and the next say
// ok, because a reader of that document cannot tell it from a flapping daemon.
func (d Deps) container() docker.Runtime {
	if d.Container != nil {
		return d.Container
	}
	var (
		once   sync.Once
		probed container.Runtime
	)
	return func(ctx context.Context) container.Runtime {
		once.Do(func() {
			probed = container.Detect(d.looker(), os.Getenv).Probe(ctx, d.runner())
		})
		return probed
	}
}

// system is the system domain's source, defaulted to the live host but wearing
// the caller's Run, so the one check in that domain that may shell out goes
// through the same seam as every other subprocess this invocation makes.
//
// The hostfs roots are not overridable through Run and there is nothing to
// derive them from, so a test that wants a staged /proc supplies the whole
// Source -- which is what the domain's own tests do, and why this only has to
// get the live case right.
func (d Deps) system() system.Source {
	if d.System != nil {
		return *d.System
	}
	src := system.New()
	if d.Run != nil {
		src.Run = d.Run
	}
	return src
}

// logs is the log-reclaim domain's source, defaulted to the live /var/log.
//
// Nothing to thread through from Run: the walk is syscalls rather than a
// subprocess, so unlike system there is no seam here that the caller's runner
// could stand in for.
func (d Deps) logs() logsqueeze.Source {
	if d.Logs != nil {
		return *d.Logs
	}
	return logsqueeze.New()
}

// Checks returns every check this build ships, grouped by domain.
//
// The order is the registration order and therefore --list's order. It is not the
// report's order -- results are sorted by id on the way out -- so this is a
// question of how the listing reads, and by domain reads the way the TUI's nav
// does.
func Checks(deps Deps) []check.Check {
	run := deps.runner()

	var out []check.Check
	out = append(out, disk.Checks(run)...)
	out = append(out, network.Checks(deps.prober(), deps.probes())...)
	out = append(out, docker.Checks(run, deps.container())...)
	out = append(out, services.Checks(run)...)
	out = append(out, system.Checks(deps.system())...)
	out = append(out, logsqueeze.Checks(deps.logs())...)
	out = append(out, storage.Checks()...)
	return out
}

// Registry returns a registry holding every check this build ships.
//
// Panics on a malformed or duplicated check, via MustRegister. That is a defect
// in this file rather than anything an operator did, it is caught by the test
// below on every run of the suite, and the alternative -- an error returned into
// a front door that has no report to put it in -- would let a build ship with a
// check silently missing.
func Registry(deps Deps) *check.Registry {
	reg := check.NewRegistry()
	reg.MustRegister(Checks(deps)...)
	return reg
}
