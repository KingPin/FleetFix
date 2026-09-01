package netprobe

import (
	"context"
	"errors"
	"testing"
	"testing/fstest"

	"github.com/KingPin/FleetFix/v2/internal/hostfs"
)

// A /proc/net/route with one default row: eth0, destination 00000000, gateway
// 0101A8C0 -- little-endian for 192.168.1.1.
const routeWithDefault = `Iface	Destination	Gateway	Flags	RefCnt	Use	Metric	Mask	MTU	Window	IRTT
eth0	00000000	0101A8C0	0003	0	0	100	00000000	0	0	0
eth0	0001A8C0	00000000	0001	0	0	100	00FFFFFF	0	0	0
`

const devWithCounters = `Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets
    lo:    1234      12    0    0    0     0          0         0     5678      34
  eth0: 9000001    4001    0    0    0     0          0         0  7000002    3002
`

func proberWithRoute(t *testing.T, route, dev, operstate string) (*Prober, *stubDialer) {
	t.Helper()
	p, _ := newTestProber(t)
	p.Host = hostfs.Host{
		Proc: fstest.MapFS{
			"net/route": {Data: []byte(route)},
			"net/dev":   {Data: []byte(dev)},
		},
		Sys: fstest.MapFS{
			"class/net/eth0/operstate": {Data: []byte(operstate)},
		},
	}
	dialer := &stubDialer{conn: &stubConn{local: stubAddr("10.0.0.5:54321")}}
	p.Dial = dialer
	return p, dialer
}

func TestNetworkSnapshotsTheDefaultRouteInterface(t *testing.T) {
	t.Parallel()
	p, _ := proberWithRoute(t, routeWithDefault, devWithCounters, "up\n")

	info := p.Network(context.Background())
	if info == nil {
		t.Fatal("no snapshot from a /proc with a default route")
	}
	if info.Iface != "eth0" {
		t.Errorf("iface = %q", info.Iface)
	}
	if info.Gateway == nil || *info.Gateway != "192.168.1.1" {
		t.Errorf("gateway = %v, want the decoded little-endian address", info.Gateway)
	}
	if info.IPv4 == nil || *info.IPv4 != "10.0.0.5" {
		t.Errorf("ipv4 = %v, want the dialer's source address without its port", info.IPv4)
	}
	if info.Operstate != "up" {
		t.Errorf("operstate = %q", info.Operstate)
	}
	// The counters must come from the row for *this* interface, not the first row
	// in the file -- lo is above eth0 and its numbers are deliberately different.
	if info.RxBytes != 9000001 || info.TxBytes != 7000002 {
		t.Errorf("counters = %d/%d, want eth0's row not lo's", info.RxBytes, info.TxBytes)
	}
}

// No default route is the diagnosis the ladder's bottom rung prints, so it has to
// arrive as nil rather than as an interface named "" that is somehow up.
func TestNetworkIsNilWithoutADefaultRoute(t *testing.T) {
	t.Parallel()
	p, _ := proberWithRoute(t, "Iface\tDestination\tGateway\n", devWithCounters, "up")
	if info := p.Network(context.Background()); info != nil {
		t.Fatalf("got a snapshot from a routeless host: %+v", info)
	}
}

// An interface with no row in /proc/net/dev is still an interface. Zeroes are the
// honest reading; dropping the snapshot would lose the operstate and the gateway.
func TestNetworkReportsZeroCountersForAnInterfaceWithNoRow(t *testing.T) {
	t.Parallel()
	p, _ := proberWithRoute(t, routeWithDefault, "Inter-|   Receive\n", "up")
	info := p.Network(context.Background())
	if info == nil {
		t.Fatal("no snapshot")
	}
	if info.RxBytes != 0 || info.TxBytes != 0 {
		t.Errorf("counters = %d/%d, want zeroes", info.RxBytes, info.TxBytes)
	}
}

// A box whose DHCP lease has not landed has a route and no address. That is a real
// state, and it must not take the whole snapshot down with it.
func TestNetworkKeepsTheSnapshotWhenThereIsNoPrimaryAddress(t *testing.T) {
	t.Parallel()
	p, _ := proberWithRoute(t, routeWithDefault, devWithCounters, "up")
	p.Dial = &stubDialer{err: errors.New("network is unreachable")}

	info := p.Network(context.Background())
	if info == nil {
		t.Fatal("no snapshot")
	}
	if info.IPv4 != nil {
		t.Errorf("ipv4 = %v, want nil", *info.IPv4)
	}
	if info.Iface != "eth0" {
		t.Errorf("iface = %q, want the route's interface reported anyway", info.Iface)
	}
}

func TestPrimaryIPv4UsesAConnectlessUDPSocket(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	conn := &stubConn{local: stubAddr("10.0.0.5:54321")}
	dialer := &stubDialer{conn: conn}
	p.Dial = dialer

	got := p.PrimaryIPv4(context.Background())
	if got == nil || *got != "10.0.0.5" {
		t.Fatalf("primary ipv4 = %v", got)
	}
	// UDP to a reserved address, or this probe is sending packets during a
	// read-only check.
	if len(dialer.calls) != 1 || dialer.calls[0] != "udp "+primaryIPv4Target {
		t.Errorf("dialed %v, want one udp dial to %s", dialer.calls, primaryIPv4Target)
	}
	if !conn.closed {
		t.Error("the socket was left open")
	}
}

func TestPrimaryIPv4IsNilWhenTheAddressIsUnreadable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		p    func(*Prober)
	}{
		{"no dialer at all", func(p *Prober) { p.Dial = nil }},
		{"dial failed", func(p *Prober) { p.Dial = &stubDialer{err: errors.New("no route")} }},
		{"no local address", func(p *Prober) { p.Dial = &stubDialer{conn: &stubConn{}} }},
		{"local address is not host:port", func(p *Prober) {
			p.Dial = &stubDialer{conn: &stubConn{local: stubAddr("nonsense")}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, _ := newTestProber(t)
			tc.p(p)
			if got := p.PrimaryIPv4(context.Background()); got != nil {
				t.Fatalf("got %q, want nil", *got)
			}
		})
	}
}

func TestResolverParsesTheFileItWasPointedAt(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	p.ResolvConf = "/run/systemd/resolve/resolv.conf"
	p.ReadFile = func(name string) (string, error) {
		if name != "/run/systemd/resolve/resolv.conf" {
			t.Errorf("read %q, want the configured path", name)
		}
		return "nameserver 10.0.0.1\nsearch corp.example\n", nil
	}

	cfg := p.Resolver()
	if len(cfg.Nameservers) != 1 || cfg.Nameservers[0] != "10.0.0.1" {
		t.Errorf("nameservers = %v", cfg.Nameservers)
	}
	if cfg.Source != "/run/systemd/resolve/resolv.conf" {
		t.Errorf("source = %q, want the path that was read", cfg.Source)
	}
}

// An unreadable resolv.conf is a finding, not a failure -- and the finding an
// operator acts on is "no nameservers in <path>", so the path has to survive.
func TestResolverReportsAnEmptyConfigNamingThePathItTried(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)

	cfg := p.Resolver()
	if len(cfg.Nameservers) != 0 {
		t.Errorf("nameservers = %v, want none", cfg.Nameservers)
	}
	if cfg.Nameservers == nil || cfg.Search == nil || cfg.Options == nil {
		t.Error("a nil slice here marshals to null; the wire format wants []")
	}
	if cfg.Source != "/etc/resolv.conf" {
		t.Errorf("source = %q", cfg.Source)
	}
}

func TestResolverFallsBackToTheDefaultPathAndReader(t *testing.T) {
	t.Parallel()
	p, _ := newTestProber(t)
	p.ResolvConf = ""
	p.ReadFile = nil

	// The real reader against the real path: whatever this host has, the answer
	// must name the default file rather than the empty string.
	if cfg := p.Resolver(); cfg.Source != DefaultResolvConf {
		t.Fatalf("source = %q, want %q", cfg.Source, DefaultResolvConf)
	}
}
