"""Tests for the /proc + /sys network identity reader."""

from __future__ import annotations

from pathlib import Path

from fleetfix.modules.network import interfaces
from fleetfix.modules.network.interfaces import (
    default_route,
    operstate,
    primary_ipv4,
    read_counters,
)

# Destination 00000000 = default route; Gateway 0102A8C0 is little-endian
# for 192.168.2.1 (C0 A8 02 01).
_ROUTE = """\
Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT
eth0\t00000000\t0102A8C0\t0003\t0\t0\t100\t00000000\t0\t0\t0
eth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0
"""

_ROUTE_NO_DEFAULT = """\
Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT
eth0\t0002A8C0\t00000000\t0001\t0\t0\t100\t00FFFFFF\t0\t0\t0
"""

_NET_DEV = """\
Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo: 1234567     100    0    0    0     0          0         0  1234567     100    0    0    0     0       0          0
  eth0: 9876543     200    0    0    0     0          0         0  5555555     300    0    0    0     0       0          0
"""


def test_default_route_iface_and_gateway(tmp_path: Path) -> None:
    f = tmp_path / "route"
    f.write_text(_ROUTE)
    assert default_route(source=f) == ("eth0", "192.168.2.1")


def test_default_route_none_when_absent(tmp_path: Path) -> None:
    f = tmp_path / "route"
    f.write_text(_ROUTE_NO_DEFAULT)
    assert default_route(source=f) is None


def test_default_route_missing_file_is_none(tmp_path: Path) -> None:
    assert default_route(source=tmp_path / "nope") is None


def test_read_counters_extracts_rx_tx(tmp_path: Path) -> None:
    f = tmp_path / "dev"
    f.write_text(_NET_DEV)
    counters = read_counters(source=f)
    assert counters["eth0"] == (9876543, 5555555)
    assert counters["lo"] == (1234567, 1234567)


def test_read_counters_missing_file_is_empty(tmp_path: Path) -> None:
    assert read_counters(source=tmp_path / "nope") == {}


def test_operstate_reads_sysfs(tmp_path: Path) -> None:
    iface_dir = tmp_path / "eth0"
    iface_dir.mkdir()
    (iface_dir / "operstate").write_text("up\n")
    assert operstate("eth0", root=tmp_path) == "up"


def test_operstate_missing_is_unknown(tmp_path: Path) -> None:
    assert operstate("eth0", root=tmp_path) == "unknown"


def test_primary_ipv4_returns_str_or_none() -> None:
    # Smoke test: no network assertion — just the contract.
    result = primary_ipv4()
    assert result is None or isinstance(result, str)


def test_read_network_none_without_default_route(monkeypatch) -> None:
    monkeypatch.setattr(interfaces, "default_route", lambda: None)
    assert interfaces.read_network() is None


def test_read_network_combines_sources(monkeypatch) -> None:
    monkeypatch.setattr(interfaces, "default_route", lambda: ("eth0", "192.168.2.1"))
    monkeypatch.setattr(interfaces, "primary_ipv4", lambda: "192.168.2.50")
    monkeypatch.setattr(interfaces, "operstate", lambda iface, root=None: "up")
    monkeypatch.setattr(interfaces, "read_counters", lambda: {"eth0": (100, 200)})
    info = interfaces.read_network()
    assert info is not None
    assert info.iface == "eth0"
    assert info.ipv4 == "192.168.2.50"
    assert info.gateway == "192.168.2.1"
    assert info.operstate == "up"
    assert (info.rx_bytes, info.tx_bytes) == (100, 200)
