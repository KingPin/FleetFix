package system

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

// aptArgs and updatesBudget are v1's, the budget read off from_apt's
// `timeout_s: int = 10` default rather than picked here.
var aptArgs = []string{"list", "--upgradable"}

const updatesBudget = 10 * time.Second

// The two places an answer can come from, named in data[] and on the metrics so a
// reader can tell a count that came from a file the notifier wrote last night
// from one apt worked out just now.
const (
	SourceNotifier = "update-notifier"
	SourceAPT      = "apt"
)

type updates struct{ src Source }

func (updates) Spec() check.Spec {
	return check.Spec{
		ID:    UpdatesID,
		Title: "Pending updates",
		// No NeedsBins for apt. The notifier fragment answers without any binary at
		// all, and it is the answer on most Ubuntu hosts -- a presence gate on apt
		// would report unavailable on a host that could have answered from a file.
		Domain:    "system",
		Budget:    updatesBudget,
		InDefault: true,
	}
}

// updateStatus is v1's UpdateStatus, minus the "unavailable" source: a status
// that carries no counts is reported as a status here, not as a row of zeros with
// a word attached.
type updateStatus struct {
	Upgradable int64  `json:"upgradable"`
	Security   int64  `json:"security"`
	Source     string `json:"source"`
}

func (c updates) Run(ctx context.Context, in check.Input) check.Result {
	rule, known := in.Thresholds.Get(threshold.UpdatesSecure)
	if !known {
		return ungraded(threshold.UpdatesSecure, "pending updates")
	}

	// The cheap answer first, exactly as v1 orders it: the notifier fragment is a
	// file read and gives the same numbers the operator saw in the MOTD at login,
	// so agreeing with what they already read is worth more than being current.
	status, ok := c.fromNotifier()
	if !ok {
		var bad check.Result
		if status, bad, ok = c.fromAPT(ctx, in); !ok {
			return bad
		}
	}

	labels := map[string]string{"source": status.Source}
	res := check.Result{
		Data: status,
		Metrics: []check.Metric{
			gauge(SecurityUpdatesMetric, float64(status.Security), "count", labels, "pending security updates"),
			gauge(UpgradableMetric, float64(status.Upgradable), "count", labels, "pending package updates"),
		},
	}
	// Security alone is graded, which is v1's policy and the defensible one: a
	// hundred pending updates on a host with a maintenance window next Tuesday is
	// not a fault, and one unpatched CVE is.
	if trip, fired := rule.Check(float64(status.Security), "security updates"); fired {
		res.Trips = append(res.Trips, trip)
	}

	res.Summary = fmt.Sprintf("%s upgradable, %d of them security (via %s)",
		plural(status.Upgradable, "package"), status.Security, status.Source)
	return res
}

// fromNotifier reads the MOTD fragment. Not found, not readable and not
// recognisable are one answer -- try apt -- because v1 collapses them too and
// because each has the same remedy.
func (c updates) fromNotifier() (updateStatus, bool) {
	if c.src.ReadFile == nil || c.src.NotifierPath == "" {
		return updateStatus{}, false
	}
	text, err := c.src.ReadFile(c.src.NotifierPath)
	if err != nil {
		return updateStatus{}, false
	}
	upgradable, security, ok := coresystem.ParseNotifierText(text)
	if !ok {
		// The routine case, not a fault: the notifier writes an empty file on a
		// host with nothing to install rather than writing a zero.
		return updateStatus{}, false
	}
	return updateStatus{Upgradable: upgradable, Security: security, Source: SourceNotifier}, true
}

// fromAPT counts what apt lists. The bool is false when there is nothing to
// count, and the Result then says why in the report's own terms.
func (c updates) fromAPT(ctx context.Context, in check.Input) (updateStatus, check.Result, bool) {
	out, err := c.src.Run.Run(ctx, "apt", aptArgs...)
	switch {
	case errors.Is(err, cmdrun.ErrNotFound):
		// unavailable, not a fault. A host with neither the notifier nor apt is a
		// host this check cannot speak about -- an RPM distribution, an immutable
		// image -- and that is a fact about the host, not a problem with it.
		return updateStatus{}, check.Result{
			Status:  check.StatusUnavailable,
			Summary: "neither update-notifier nor apt can answer on this host",
		}, false
	case err != nil:
		return updateStatus{}, check.Result{
			Status:  check.StatusError,
			Summary: "apt list --upgradable did not run",
			Error:   err.Error(),
		}, false
	case !out.OK():
		return updateStatus{}, check.Result{
			Status:  check.StatusError,
			Summary: "apt list --upgradable failed",
			Error:   firstLine(out.Combined(), fmt.Sprintf("exited %d", out.ExitCode)),
		}, false
	}

	// apt prints its "Listing... Done" progress line and, on a host with an
	// unreachable mirror, a WARNING to stderr while still exiting 0 and listing
	// what it knows. That is worth showing without downgrading the host, for the
	// reason df's grumble is.
	if grumble := firstLine(out.Stderr, ""); grumble != "" {
		in.Progress.Emit(check.Event{Text: "apt: " + grumble, Status: check.StatusWarn})
	}

	upgradable, security := coresystem.ParseAptUpgradable(out.Stdout)
	return updateStatus{Upgradable: upgradable, Security: security, Source: SourceAPT}, check.Result{}, true
}
