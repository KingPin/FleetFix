package network

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/check"
	"github.com/KingPin/FleetFix/v2/internal/hostfs"
)

func TestInterfaceReportsTheLinkAndItsCounters(t *testing.T) {
	t.Parallel()
	h := newHost(t)

	res, _ := h.runID(t, InterfaceID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	if res.Summary != "eth0 10.0.0.5 via 192.168.1.1 (up)" {
		t.Errorf("summary = %q", res.Summary)
	}

	// Counters, not gauges. These are the kernel's cumulative totals and reset
	// with the interface; graphing them as a gauge shows lifetime bytes where a
	// dashboard means throughput.
	for _, m := range res.Metrics {
		if m.Kind != check.Counter {
			t.Errorf("%s is a %s, want a counter", m.Name, m.Kind)
		}
		if m.Labels["iface"] != "eth0" {
			t.Errorf("%s is not labelled by interface: %v", m.Name, m.Labels)
		}
	}
	if len(res.Metrics) != 2 {
		t.Fatalf("got %d metrics", len(res.Metrics))
	}
	if res.Metrics[0].Value != 9000001 || res.Metrics[1].Value != 7000002 {
		t.Errorf("counters = %v / %v", res.Metrics[0].Value, res.Metrics[1].Value)
	}
}

// A host with no default route has no interface to report on, and the absence
// is the diagnosis rather than a missing reading.
func TestInterfaceReportsNoDefaultRoute(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.Host = hostfs.Host{Proc: fstest.MapFS{}, Sys: fstest.MapFS{}}

	res, _ := h.runID(t, InterfaceID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Summary != "no default route — this box has no path off itself" {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestInterfaceFailsOnlyOnADownLink(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		operstate string
		want      check.Status
	}{
		{"up", check.StatusOK},
		// "unknown" is what wireguard, tun/tap and some virtio links report while
		// passing traffic, and is also Operstate's own fallback when the sysfs
		// read fails. Failing on it would put a crit on a fleet of healthy hosts.
		{"unknown", check.StatusOK},
		{"dormant", check.StatusOK},
		{"down", check.StatusCrit},
	} {
		t.Run(tc.operstate, func(t *testing.T) {
			t.Parallel()
			h := newHost(t)
			h.prober.Host = hostfs.Host{
				Proc: fstest.MapFS{
					"net/route": {Data: []byte(routeWithDefault)},
					"net/dev":   {Data: []byte(devWithCounters)},
				},
				Sys: fstest.MapFS{
					"class/net/eth0/operstate": {Data: []byte(tc.operstate + "\n")},
				},
			}

			res, _ := h.runID(t, InterfaceID, nil)
			if res.Status != tc.want {
				t.Fatalf("operstate %q → %q, want %q (%s)", tc.operstate, res.Status, tc.want, res.Summary)
			}
		})
	}
}

// A link that is up with no address routes nothing, but that is the normal
// state of a bridge member or a bond slave -- so it warns rather than fails,
// and the ladder is what says whether it mattered.
func TestInterfaceWarnsOnALinkWithNoAddress(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.Dial = dialFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("network is unreachable")
	})

	res, _ := h.runID(t, InterfaceID, nil)
	if res.Status != check.StatusWarn {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	if !strings.Contains(res.Summary, "no IPv4 address") {
		t.Errorf("summary = %q", res.Summary)
	}
}

func TestResolverReportsTheNameserversInFileOrder(t *testing.T) {
	t.Parallel()
	h := newHost(t)

	res, _ := h.runID(t, ResolverID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	// File order, not sorted: the resolver asks them in that order, so reordering
	// would misreport which server gets asked first.
	if res.Summary != "192.168.1.1, 1.1.1.1  search lan" {
		t.Errorf("summary = %q", res.Summary)
	}
}

// Nothing on the host can resolve. Named here rather than left to network.dns,
// because this is the cause and a failed lookup is only the symptom.
func TestResolverFailsWhenThereAreNoNameservers(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.ReadFile = func(string) (string, error) { return "search lan\n", nil }

	res, _ := h.runID(t, ResolverID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Summary != "no nameservers in /etc/resolv.conf" {
		t.Errorf("summary = %q, want the path named", res.Summary)
	}
}

// An unreadable resolv.conf is the same finding as an empty one: nothing can
// resolve, and the path is what the operator needs.
func TestResolverFailsWhenTheFileCannotBeRead(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.ReadFile = func(string) (string, error) { return "", errors.New("permission denied") }

	res, _ := h.runID(t, ResolverID, nil)
	if res.Status != check.StatusCrit {
		t.Fatalf("status = %q", res.Status)
	}
}

// The addresses that actually answer sit behind the stub, so reporting
// 127.0.0.53 alone sends an operator hunting a broken loopback.
func TestResolverNamesTheSystemdStubAndWhereToLookBehindIt(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.prober.ReadFile = func(string) (string, error) { return "nameserver 127.0.0.53\n", nil }

	res, _ := h.runID(t, ResolverID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q", res.Status)
	}
	if !strings.Contains(res.Summary, "resolvectl status") {
		t.Errorf("summary = %q, want the command that shows the upstreams", res.Summary)
	}
}

func TestSocketsInventoriesWhatIsListening(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(ssListeners, "ss", "-tlnpH")

	res, _ := h.runID(t, SocketsID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q: %s", res.Status, res.Summary)
	}
	if res.Summary != "2 listening TCP ports" {
		t.Errorf("summary = %q", res.Summary)
	}
	if len(res.Metrics) != 1 || res.Metrics[0].Value != 2 {
		t.Errorf("metrics = %+v", res.Metrics)
	}
}

// One port is a port, not "1 ports". An inventory line is read by a human.
func TestSocketsCountsOnePortInTheSingular(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout(
		`LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*    users:(("sshd",pid=812,fd=3))`,
		"ss", "-tlnpH",
	)

	res, _ := h.runID(t, SocketsID, nil)
	if res.Summary != "1 listening TCP port" {
		t.Errorf("summary = %q", res.Summary)
	}
}

// An inventory has no failing value: which ports ought to be open is a fleet
// policy question this tool has no opinion about, and a host deliberately
// listening on nothing is not broken.
func TestSocketsIsOKWithNothingListening(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Stdout("", "ss", "-tlnpH")

	res, _ := h.runID(t, SocketsID, nil)
	if res.Status != check.StatusOK {
		t.Fatalf("status = %q", res.Status)
	}
	if res.Summary != "0 listening TCP ports" {
		t.Errorf("summary = %q", res.Summary)
	}
}

// The whole reason netprobe.Sockets returns an error rather than v1's empty
// list: unavailable and "nothing listening" are different answers, and only one
// of them means the host has no services.
func TestSocketsSeparatesAMissingSSFromABrokenOne(t *testing.T) {
	t.Parallel()
	t.Run("not installed", func(t *testing.T) {
		t.Parallel()
		h := newHost(t)
		h.run.Missing("ss", "-tlnpH")

		res, _ := h.runID(t, SocketsID, nil)
		if res.Status != check.StatusUnavailable {
			t.Fatalf("status = %q, want unavailable", res.Status)
		}
	})
	t.Run("would not run", func(t *testing.T) {
		t.Parallel()
		h := newHost(t)
		h.run.Fail(errors.New("fork/exec: permission denied"), "ss", "-tlnpH")

		res, _ := h.runID(t, SocketsID, nil)
		if res.Status != check.StatusError {
			t.Fatalf("status = %q, want error", res.Status)
		}
		if res.Error == "" {
			t.Error("no reason recorded on an errored check")
		}
	})
}

// ss exits 1 having printed every row it could when it cannot read the process
// table for some sockets, which is routine unprivileged. Those rows are the
// answer.
func TestSocketsKeepsTheRowsFromANonZeroExit(t *testing.T) {
	t.Parallel()
	h := newHost(t)
	h.run.Exit(1, ssListeners, "Cannot open netlink socket", "ss", "-tlnpH")

	res, _ := h.runID(t, SocketsID, nil)
	if res.Status != check.StatusOK || res.Summary != "2 listening TCP ports" {
		t.Fatalf("status = %q, summary = %q", res.Status, res.Summary)
	}
}

// The three cheap checks answer on a host with no networking tools at all,
// which is the state of most minimal containers. Only sockets needs a binary.
func TestTheFactsChecksNeedAlmostNothingInstalled(t *testing.T) {
	t.Parallel()
	needs := map[check.ID][]string{}
	for _, c := range Checks(nil, probes()) {
		needs[c.Spec().ID] = c.Spec().NeedsBins
	}
	if len(needs[InterfaceID]) != 0 || len(needs[ResolverID]) != 0 {
		t.Errorf("interface needs %v, resolver needs %v", needs[InterfaceID], needs[ResolverID])
	}
	if len(needs[SocketsID]) != 1 || needs[SocketsID][0] != "ss" {
		t.Errorf("sockets needs %v", needs[SocketsID])
	}
}
