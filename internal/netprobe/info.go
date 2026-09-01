package netprobe

import (
	"context"
	"net"

	"github.com/KingPin/FleetFix/v2/internal/core/network"
)

// primaryIPv4Target is the address the source-address trick asks the kernel about.
//
// TEST-NET-1 (RFC 5737) is reserved for documentation and is never routed, and a
// connected UDP socket sends nothing -- connect(2) on SOCK_DGRAM only makes the
// kernel pick the source address it *would* use. So this touches no network even
// on a box with a live default route, which is what makes it safe to run inside a
// read-only check.
const primaryIPv4Target = "192.0.2.1:9"

// Network snapshots the default-route interface, or nil when there is no default
// route.
//
// nil rather than a zero Info: "this box has no path off itself" is the diagnosis
// the ladder's bottom rung prints, and a zero-valued Info would render as an
// interface named "" that is somehow up.
func (p *Prober) Network(ctx context.Context) *network.Info {
	iface, gateway, ok := network.DefaultRoute(p.Host.Proc, network.ProcNetRoute)
	if !ok {
		return nil
	}
	counters := network.ReadCounters(p.Host.Proc, network.ProcNetDev)[iface]
	return &network.Info{
		Iface:     iface,
		IPv4:      p.PrimaryIPv4(ctx),
		Gateway:   &gateway,
		Operstate: network.Operstate(p.Host.Sys, network.SysClassNet, iface),
		RxBytes:   counters.RxBytes,
		TxBytes:   counters.TxBytes,
	}
}

// PrimaryIPv4 is the source address this box would use to reach the internet, or
// nil when the kernel will not name one.
//
// nil is a real answer, not a failure: a host with no route to anywhere has no
// primary address, and so does one whose DHCP lease has not landed yet. Reporting
// either as an error would make a booting box look broken.
func (p *Prober) PrimaryIPv4(ctx context.Context) *string {
	if p.Dial == nil {
		return nil
	}
	conn, err := p.Dial.DialContext(ctx, "udp", primaryIPv4Target)
	if err != nil {
		return nil
	}
	defer func() { _ = conn.Close() }()

	local := conn.LocalAddr()
	if local == nil {
		return nil
	}
	host, _, err := net.SplitHostPort(local.String())
	if err != nil {
		// A LocalAddr that is not host:port is not something a UDP socket
		// produces; reporting no address beats reporting a malformed one.
		return nil
	}
	return &host
}

// Resolver reads resolv.conf.
//
// An unreadable file yields an empty config carrying the path it tried, not an
// error: "no nameservers in /etc/resolv.conf" is the finding, and the path is the
// half of it an operator acts on.
func (p *Prober) Resolver() network.ResolverConfig {
	source := p.ResolvConf
	if source == "" {
		source = DefaultResolvConf
	}
	read := p.ReadFile
	if read == nil {
		read = readOSFile
	}
	text, err := read(source)
	if err != nil {
		// Parsing nothing rather than building the empty config by hand: a file
		// with no directives in it is the same answer, and writing it out twice is
		// how the two spellings drift.
		text = ""
	}
	return network.ParseResolvConf(text, source)
}
