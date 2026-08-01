package network

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/cmdrun"
	"github.com/KingPin/FleetFix/v2/internal/netprobe"
)

// The three checks that read what the host already knows, rather than probing
// anything: the link, the resolver configuration, and what is listening. All
// three are cheap, so all three are in the default run.

// factsBudget covers a handful of /proc and /sys reads and one file read. Not
// zero, because a read against a stale NFS-mounted anything can block, and a
// check that hangs is worse than one that reports it could not look.
const factsBudget = 5 * time.Second

// socketsBudget is netprobe's own 5s on ss, plus the slack. ss reads a netlink
// table; anything approaching this means something is badly wrong.
const socketsBudget = 5*time.Second + slack

type iface struct{ p *netprobe.Prober }

func (iface) Spec() check.Spec {
	return check.Spec{
		ID:     InterfaceID,
		Title:  "Primary interface",
		Domain: "network",
		// No NeedsBins: this is /proc/net/route, /proc/net/dev and
		// /sys/class/net, which is why it still answers on a host with no
		// iproute2 -- and why it is the one network check a minimal container
		// can complete.
		Budget:    factsBudget,
		InDefault: true,
	}
}

func (c iface) Run(ctx context.Context, in check.Input) check.Result {
	info := c.p.Network(ctx)
	if info == nil {
		// v1's _format_link wording. The absence is the diagnosis: there is no
		// interface to report on because there is no route off this box.
		return check.Result{
			Status:  check.StatusCrit,
			Summary: "no default route — this box has no path off itself",
		}
	}

	labels := map[string]string{"iface": info.Iface}
	res := check.Result{
		Data: info,
		Metrics: []check.Metric{
			// Counters, not gauges: these are the kernel's cumulative totals and
			// they reset when the interface does. A dashboard has to rate() them,
			// and marking them gauge is how a scrape ends up graphing lifetime
			// bytes as though it were throughput.
			counter(RxBytesMetric, float64(info.RxBytes), "bytes", labels, "bytes received on the primary interface"),
			counter(TxBytesMetric, float64(info.TxBytes), "bytes", labels, "bytes sent on the primary interface"),
		},
		Summary: fmt.Sprintf("%s %s via %s (%s)",
			info.Iface, orText(info.IPv4, "no address"), orText(info.Gateway, "no gateway"), info.Operstate),
	}

	// Only "down" fails, for the reason linkRung gives at length: operstate reads
	// "unknown" both for links that are up and passing traffic but whose driver
	// never reports carrier, and as the sysfs read's own fallback. Failing on it
	// would put a crit on every wireguard and tun/tap host in the fleet.
	if info.Operstate == "down" {
		res.Status = check.StatusCrit
		res.Summary = fmt.Sprintf("%s is down", info.Iface)
		return res
	}
	if info.IPv4 == nil || *info.IPv4 == "" {
		// A link that is up with no address routes nothing. Warn rather than
		// crit: this is the normal state of a bridge member or a bond slave, and
		// the ladder is what says whether it actually mattered.
		res.Status = check.StatusWarn
		res.Summary = fmt.Sprintf("%s is %s with no IPv4 address", info.Iface, info.Operstate)
		return res
	}
	res.Status = check.StatusOK
	return res
}

type resolver struct{ p *netprobe.Prober }

func (resolver) Spec() check.Spec {
	return check.Spec{
		ID:        ResolverID,
		Title:     "DNS resolver configuration",
		Domain:    "network",
		Budget:    factsBudget,
		InDefault: true,
	}
}

func (c resolver) Run(_ context.Context, _ check.Input) check.Result {
	cfg := c.p.Resolver()
	res := check.Result{Data: cfg}

	if len(cfg.Nameservers) == 0 {
		// Nothing can resolve. Reported here rather than left to network.dns
		// because this names the cause -- an empty resolv.conf -- where a failed
		// lookup only shows the symptom.
		res.Status = check.StatusCrit
		res.Summary = "no nameservers in " + cfg.Source
		return res
	}

	servers := strings.Join(cfg.Nameservers, ", ")
	res.Status = check.StatusOK
	if cfg.StubResolver() {
		// v1's wording, and its reason: the addresses that actually answer sit
		// behind the stub, so reporting "127.0.0.53" alone would send an operator
		// hunting a broken loopback.
		res.Summary = servers + " (systemd-resolved stub — upstreams: resolvectl status)"
		return res
	}
	res.Summary = servers
	if len(cfg.Search) > 0 {
		res.Summary += "  search " + strings.Join(cfg.Search, " ")
	}
	return res
}

type sockets struct{ p *netprobe.Prober }

func (sockets) Spec() check.Spec {
	return check.Spec{
		ID:        SocketsID,
		Title:     "Listening TCP sockets",
		Domain:    "network",
		NeedsBins: []string{"ss"},
		Budget:    socketsBudget,
		InDefault: true,
	}
}

func (c sockets) Run(ctx context.Context, _ check.Input) check.Result {
	socks, err := c.p.Sockets(ctx)
	switch {
	case errors.Is(err, cmdrun.ErrNotFound):
		// The runner's NeedsBins gate answers this first on any normal path. Kept
		// because a Runner with no Looker gates nothing, and because ss can be
		// removed between the lookup and the call.
		return check.Result{
			Status:  check.StatusUnavailable,
			Summary: "ss is not installed on this host",
		}
	case err != nil:
		return check.Result{
			Status:  check.StatusError,
			Summary: "ss did not run",
			Error:   err.Error(),
		}
	}

	// ok whatever the count. An inventory has no failing value: which ports ought
	// to be open is a fleet policy question this tool has no opinion about, and a
	// host that is deliberately listening on nothing is not broken. The finding is
	// the list, which is in data[] and in the metric.
	return check.Result{
		Status: check.StatusOK,
		Data:   socks,
		Metrics: []check.Metric{
			gauge(ListenersMetric, float64(len(socks)), "count",
				map[string]string{}, "TCP ports in LISTEN"),
		},
		Summary: plural(len(socks), "listening TCP port"),
	}
}

// orText is Python's `x or fallback` over an optional string: a nil pointer and
// an empty string are the same non-answer.
func orText(s *string, fallback string) string {
	if s == nil || *s == "" {
		return fallback
	}
	return *s
}

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
