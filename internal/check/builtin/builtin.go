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
	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/check/builtin/disk"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
)

// Deps is everything the collectors need from the host, in the shape that lets a
// test stage all of it.
//
// A struct rather than a bare Runner even with one field in it: the collectors
// still to land need a filesystem, a dialer and a resolver, and every one of
// those arriving as a new positional argument would touch every call site again.
type Deps struct {
	// Run is the subprocess seam. Nil means the real one, so a caller that only
	// wants to know what this build can check does not have to supply one.
	Run cmdrun.Runner
}

func (d Deps) runner() cmdrun.Runner {
	if d.Run == nil {
		return cmdrun.New()
	}
	return d.Run
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
