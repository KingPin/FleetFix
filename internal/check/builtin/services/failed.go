package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coreservices "github.com/KingPin/FleetFix/v2/internal/core/services"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// systemctlBin and the two argument lists are v1's, and the first is
// load-bearing: --plain --no-legend is the shape internal/core/services.
// ParseFailedUnits was ported against and the shape the differential corpus was
// captured in.
const systemctlBin = "systemctl"

var (
	listArgs = []string{"list-units", "--state=failed", "--no-legend", "--no-pager", "--plain"}
	showArgs = []string{"show", "-p", "User"}
)

// A Unit is one failed unit: what systemctl listed, plus who it runs as.
//
// This is the shape of data[] for services.failed, and best-effort like every
// data[] -- consumers pin on metrics[].
type Unit struct {
	coreservices.FailedUnit

	// Owner is the unit's User=, defaulted to root the way systemd does. Empty
	// means the second call did not answer, which is not the same as root and must
	// not be rendered as it.
	Owner string `json:"owner"`
}

type failed struct{ run cmdrun.Runner }

func (failed) Spec() check.Spec {
	return check.Spec{
		ID:        FailedID,
		Title:     "Failed units",
		Domain:    "services",
		NeedsBins: []string{systemctlBin},
		Budget:    failedBudget,
		InDefault: true,
	}
}

func (c failed) Run(ctx context.Context, in check.Input) check.Result {
	rule, known := in.Thresholds.Get(threshold.ServicesFailed)
	if !known {
		return ungraded(threshold.ServicesFailed, "failed units")
	}

	out, err := c.run.Run(ctx, systemctlBin, listArgs...)
	switch {
	case errors.Is(err, cmdrun.ErrNotFound):
		return notManagedBySystemd(systemctlBin)
	case err != nil:
		return check.Result{
			Status:  check.StatusError,
			Summary: "systemctl list-units did not run",
			Error:   err.Error(),
		}
	case !out.OK():
		// Not unavailable. systemctl is installed and refused, which on a host with
		// a dead or unreachable systemd is a real fault -- v1 returned an empty list
		// here and drew a pane saying nothing had failed.
		return check.Result{
			Status:  check.StatusError,
			Summary: "systemctl list-units failed",
			Error:   firstLine(out.Combined(), fmt.Sprintf("exited %d", out.ExitCode)),
		}
	}

	rows := unitsFrom(coreservices.ParseFailedUnits(out.Stdout))
	if len(rows) == 0 {
		return check.Result{
			Status:  check.StatusOK,
			Summary: "no failed units",
			Data:    []Unit{},
			Metrics: []check.Metric{failedGauge(0)},
		}
	}

	c.owners(ctx, rows, in.Progress)

	res := check.Result{Data: rows, Metrics: []check.Metric{failedGauge(len(rows))}}
	for _, row := range rows {
		// A step per failed unit, which is the one place in this domain where a step
		// per row is right: the count is what gets graded, and the names are what
		// the operator does something about.
		//
		// Emitted rather than appended to res.Steps, because the runner keeps a
		// check's own Steps if it set any -- building the slice here would silently
		// drop the warning owners() may already have emitted above it.
		in.Progress.Emit(check.Event{Text: describe(row), Status: check.StatusWarn})
	}
	if trip, fired := rule.Check(float64(len(rows)), "failed units"); fired {
		res.Trips = append(res.Trips, trip)
	}

	// Every name, not the first few. A host with twenty failed units has a summary
	// worth its length, and the alternative -- "and 17 more" -- is a line that
	// tells an operator to go and run systemctl themselves.
	res.Summary = plural(len(rows), "failed unit") + ": " + strings.Join(names(rows), ", ")
	return res
}

// owners fills in each unit's User=, for every unit at once.
//
// One bulk call, which is v1's shape too. What differs is the failure: v1 used
// this to filter and returned an empty list when the reply did not line up, so a
// mismatch there meant "nothing has failed". Here it annotates, so a mismatch
// costs the owners and keeps the units -- the count is already graded and the
// names are already worth reporting.
//
// The join is by position, because systemctl prints one block per unit in the
// order asked. A reply with the wrong number of blocks is refused wholesale
// rather than matched up hopefully: an owner attributed to the wrong unit would
// send someone to the wrong person.
func (c failed) owners(ctx context.Context, rows []Unit, to check.Emitter) {
	args := append(append([]string{}, showArgs...), names(rows)...)
	out, err := c.run.Run(ctx, systemctlBin, args...)
	switch {
	case err != nil:
		c.noOwners(to, err.Error())
		return
	case !out.OK():
		c.noOwners(to, firstLine(out.Combined(), fmt.Sprintf("exited %d", out.ExitCode)))
		return
	}

	users := coreservices.ParseShowUser(out.Stdout)
	if len(users) != len(rows) {
		c.noOwners(to, fmt.Sprintf("systemctl described %d of %d units", len(users), len(rows)))
		return
	}
	for i := range rows {
		rows[i].Owner = users[i]
	}
}

// noOwners records that the owners are missing.
//
// A step rather than a status. The listing answered, so the failed units are real
// and the grade stands; what is lost is which of them are one operator's problem
// and which are the host's.
func (c failed) noOwners(to check.Emitter, why string) {
	to.Emit(check.Event{
		Text:   "unit owners are unavailable: " + why,
		Status: check.StatusWarn,
	})
}

func unitsFrom(parsed []coreservices.FailedUnit) []Unit {
	rows := make([]Unit, len(parsed))
	for i, u := range parsed {
		rows[i] = Unit{FailedUnit: u}
	}
	return rows
}

// describe is one unit's line in steps[]. The description is systemd's own human
// name for the unit and is usually more use than the unit name; the owner is
// appended only when it is known, since an absent one is not root.
func describe(u Unit) string {
	line := u.Name
	if u.Description != "" {
		line += " (" + u.Description + ")"
	}
	line += " is " + u.Sub
	if u.Owner != "" {
		line += ", running as " + u.Owner
	}
	return line
}

// names is every unit's name, in the order systemctl listed them -- the argument
// list for the second call, and the summary's tail once joined.
func names(rows []Unit) []string {
	out := make([]string, len(rows))
	for i, row := range rows {
		out[i] = row.Name
	}
	return out
}

func failedGauge(n int) check.Metric {
	return gauge(FailedMetric, float64(n), "count", "systemd units in the failed state")
}
