package system

import (
	"context"
	"fmt"

	"github.com/KingPin/FleetFix/v2/internal/check"
	coresystem "github.com/KingPin/FleetFix/v2/internal/core/system"
	"github.com/KingPin/FleetFix/v2/internal/threshold"
)

type thermal struct{ src Source }

func (thermal) Spec() check.Spec {
	return check.Spec{
		ID:        ThermalID,
		Title:     "Temperature",
		Domain:    "system",
		Budget:    readBudget,
		InDefault: true,
	}
}

func (c thermal) Run(ctx context.Context, in check.Input) check.Result {
	rule, known := in.Thresholds.Get(threshold.ThermalTempC)
	if !known {
		return ungraded(threshold.ThermalTempC, "temperature")
	}

	zones, err := coresystem.ReadZones(c.src.Host.Sys, coresystem.SysThermal)
	if err != nil {
		return unreadable("/sys/"+coresystem.SysThermal, err)
	}

	hottest, any := coresystem.Hottest(zones)
	if !any {
		// unavailable, not ok and not a fault. Every VM and every container reports
		// no thermal zones, and that is most of a fleet -- a status that reached the
		// exit code would make the common case red, and an ok would claim this host
		// is running cool when nothing measured it.
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: "this host reports no thermal sensors",
			Data:    zones,
		}
	}

	res := check.Result{Data: zones}
	for _, zone := range zones {
		// Labelled by both, because neither identifies a sensor on its own: the
		// name is stable across a boot and meaningless to a human, and the type is
		// readable and repeats on a multi-socket host.
		res.Metrics = append(res.Metrics, gauge(
			TempMetric, zone.TempC, "C",
			map[string]string{"zone": zone.Name, "type": zone.Type},
			"thermal zone temperature",
		))
	}

	// Graded on the hottest zone alone, which is v1's rule and the right one: a
	// host throttles on whichever sensor crosses first, and averaging a hot CPU
	// package against a cool chassis is how a thermal problem stays green.
	if trip, fired := rule.Check(hottest.TempC, hottest.Type); fired {
		res.Trips = append(res.Trips, trip)
	}

	res.Summary = fmt.Sprintf("%s, hottest is %s at %.1fC",
		plural(int64(len(zones)), "thermal zone"), hottest.Type, hottest.TempC)
	return res
}
