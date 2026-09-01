package resolve

import (
	"fmt"

	"github.com/KingPin/FleetFix/v2/internal/config"
	corenet "github.com/KingPin/FleetFix/v2/internal/core/network"
)

// probes.yml is resolved here rather than behind a config.Paths method like
// thresholds.yml, and the reason is a compiler one: the resolver lives in
// internal/core/network, which already imports internal/config for PyRepr and
// ParseHostPort. A Paths.Probes() would close that loop into an import cycle.
//
// The consequence is that this is the one config file whose warnings are worded
// outside the package that produced them. corenet.Warning is a code plus the
// operator's own value, deliberately -- v1 only ever logged these, so there is no
// wording to be faithful to -- and the prose belongs here, next to the other
// files' warnings, where the phrasing they share is visible in one place.

// resolveProbes merges probes.yml over the defaults and folds what it complained
// about into the file's own warnings, so config_warnings[] stays in Files order.
func resolveProbes(paths config.Paths) (corenet.Probes, config.Loaded) {
	loaded := paths.Load(config.ProbesFile)
	probes, warnings := corenet.ResolveProbes(loaded.Values)
	for _, w := range warnings {
		loaded.Warnings = append(loaded.Warnings, config.ProbesFile+": "+describeProbeWarning(w))
	}
	return probes, loaded
}

// describeProbeWarning is one adjusted or ignored value, said the way the other
// files say theirs: where it was, what was there, and what happened instead.
//
// Every line ends with what the tool did, because the failure this guards against
// is an operator who set a target list and never found out it was refused -- a
// probe silently running against the defaults is a check that measures a host
// nobody asked about.
func describeProbeWarning(w corenet.Warning) string {
	where := w.Section
	if w.Key != "" {
		where += "." + w.Key
	}
	// Got and Used are already Python reprs, so a value's type survives into the
	// message: an operator who wrote `count: "5"` needs the quotes to understand
	// why their number was refused.
	switch w.Kind {
	case corenet.WarnNotAMapping:
		return fmt.Sprintf("%s is not a mapping (got %s), ignoring the section", where, w.Got)
	case corenet.WarnNotAList:
		return fmt.Sprintf("%s is not a list (got %s), using %s", where, w.Got, w.Used)
	case corenet.WarnNotANumber:
		return fmt.Sprintf("%s is not a number (got %s), using %s", where, w.Got, w.Used)
	case corenet.WarnClamped:
		return fmt.Sprintf("%s %s is out of range, using %s", where, w.Got, w.Used)
	case corenet.WarnBadTarget:
		return fmt.Sprintf("%s has no port (got %s), dropping it", where, w.Got)
	default:
		// A kind added to the resolver without a sentence here. Still says where
		// and what, so the operator is told something rather than nothing.
		return fmt.Sprintf("%s: %s (got %s)", where, w.Kind, w.Got)
	}
}
