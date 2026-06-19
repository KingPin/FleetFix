"""Basic network identity from /proc and /sys — no subprocess, no psutil.

The detailed network *screen* runs ping/dns/socket probes; this module answers
the dashboard's much cheaper question: "which interface am I on, what's my IP,
who's my gateway, is the link up, and how much is it moving?" Everything here is
a plain file read (plus one connect-less UDP socket for the primary IP), so it's
safe to call on the dashboard's 2-second tick.

Throughput is intentionally *not* computed here: this module reports the raw
cumulative rx/tx byte counters and the caller derives a rate from two snapshots
and the elapsed time. That keeps the module stateless and unit-testable.
"""

from __future__ import annotations

import socket
from dataclasses import dataclass
from pathlib import Path

_PROC_ROUTE = Path("/proc/net/route")
_PROC_NET_DEV = Path("/proc/net/dev")
_SYS_CLASS_NET = Path("/sys/class/net")


@dataclass(frozen=True)
class NetworkInfo:
    iface: str
    ipv4: str | None
    gateway: str | None
    operstate: str
    rx_bytes: int
    tx_bytes: int


def _hex_le_to_ipv4(h: str) -> str:
    """Convert a little-endian hex word (as /proc/net/route stores it) to dotted IPv4."""
    octets = bytes.fromhex(h)
    return ".".join(str(b) for b in reversed(octets))


def default_route(source: Path = _PROC_ROUTE) -> tuple[str, str] | None:
    """Return ``(iface, gateway_ipv4)`` for the default route, or None if there isn't one.

    /proc/net/route is tab/space separated with a header row. The default route
    is the entry whose Destination field is all zeroes (``00000000``).
    """
    try:
        text = source.read_text()
    except OSError:
        return None
    for line in text.splitlines():
        parts = line.split()
        if len(parts) < 3 or parts[0] == "Iface":
            continue
        iface, destination, gateway = parts[0], parts[1], parts[2]
        if destination != "00000000":
            continue
        try:
            return iface, _hex_le_to_ipv4(gateway)
        except ValueError:
            return None
    return None


def primary_ipv4() -> str | None:
    """Best-effort primary outbound IPv4 via a connect-less UDP socket.

    Connecting a UDP socket sends no packets; it just makes the kernel pick the
    source address it *would* use to reach the target, which is the address bound
    to the default-route interface. TEST-NET-1 (192.0.2.0/24) is reserved and
    never routed, so this never touches the network.
    """
    sock = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    try:
        sock.connect(("192.0.2.1", 9))
        return str(sock.getsockname()[0])
    except OSError:
        return None
    finally:
        sock.close()


def operstate(iface: str, root: Path = _SYS_CLASS_NET) -> str:
    """Read /sys/class/net/<iface>/operstate ('up'/'down'/'unknown'/...)."""
    try:
        return (root / iface / "operstate").read_text().strip()
    except OSError:
        return "unknown"


def read_counters(source: Path = _PROC_NET_DEV) -> dict[str, tuple[int, int]]:
    """Parse /proc/net/dev into ``{iface: (rx_bytes, tx_bytes)}``.

    The first two lines are headers. Each data row is ``iface: <8 rx cols>
    <8 tx cols>``; rx_bytes is the first receive column, tx_bytes the first
    transmit column (index 8 of the numeric fields).
    """
    out: dict[str, tuple[int, int]] = {}
    try:
        text = source.read_text()
    except OSError:
        return out
    for line in text.splitlines():
        name, sep, rest = line.partition(":")
        if not sep:
            continue
        iface = name.strip()
        nums = rest.split()
        if len(nums) < 9:
            continue
        try:
            out[iface] = (int(nums[0]), int(nums[8]))
        except ValueError:
            continue
    return out


def read_network() -> NetworkInfo | None:
    """Snapshot the default-route interface, or None if there's no default route."""
    route = default_route()
    if route is None:
        return None
    iface, gateway = route
    rx_tx = read_counters().get(iface, (0, 0))
    return NetworkInfo(
        iface=iface,
        ipv4=primary_ipv4(),
        gateway=gateway,
        operstate=operstate(iface),
        rx_bytes=rx_tx[0],
        tx_bytes=rx_tx[1],
    )
