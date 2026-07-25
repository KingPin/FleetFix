"""Pilot tests for the Network view — all subprocess + socket calls are stubbed."""

from __future__ import annotations

import socket
import threading
from pathlib import Path
from typing import Any

import pytest
from textual.widgets import DataTable, Input, Static, TabbedContent, TabPane

from fleetfix.app import FleetFixApp
from fleetfix.modules.network.curl_probe import CurlProbe
from fleetfix.modules.network.interfaces import NetworkInfo
from fleetfix.modules.network.ping import PingSummary
from fleetfix.modules.network.resolver import ResolverConfig
from fleetfix.modules.network.sockets import ListeningSocket
from fleetfix.modules.network.tcp import TcpCheck, TcpTarget
from fleetfix.modules.network.traceroute import TraceHop, TraceResult


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


@pytest.fixture(autouse=True)
def _stub_listening_sockets(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "fleetfix.screens.network.list_listening_sockets",
        lambda: [
            ListeningSocket(local_address="0.0.0.0", local_port=22, process_name="sshd", pid=812),
            ListeningSocket(
                local_address="127.0.0.1", local_port=5432, process_name="postgres", pid=1234
            ),
        ],
    )


_UP = NetworkInfo(
    iface="eth0", ipv4="10.0.0.9", gateway="10.0.0.1", operstate="up", rx_bytes=1, tx_bytes=2
)


def _resolver(*nameservers: str) -> ResolverConfig:
    return ResolverConfig(
        nameservers=nameservers,
        search=("corp.internal",),
        options=(),
        source="/etc/resolv.conf",
    )


@pytest.fixture
def _stub_facts(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.screens.network.read_network", lambda: _UP)
    monkeypatch.setattr("fleetfix.screens.network.read_resolver", lambda: _resolver("10.0.0.53"))


@pytest.mark.asyncio
async def test_sockets_table_populates_on_mount() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        table = app.query_one("#sockets-table", DataTable)
        assert table.row_count == 2


@pytest.mark.asyncio
async def test_curl_probe_renders_timing(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_curl(url: str, **_: Any) -> CurlProbe:
        return CurlProbe(
            url=url,
            ok=True,
            http_code=200,
            time_total_s=0.123,
            time_namelookup_s=0.001,
            time_connect_s=0.012,
            time_appconnect_s=0.045,
            time_starttransfer_s=0.099,
            size_download_bytes=4096,
        )

    monkeypatch.setattr("fleetfix.screens.network.run_curl", fake_curl)
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        app.query_one("#probe-target", Input).value = "https://api.internal/health"
        await pilot.click("#probe-curl")
        await app.workers.wait_for_complete()
        await pilot.pause()
        result = str(app.query_one("#probe-result", Static).render())
        assert "HTTP 200" in result
        assert "123.0ms" in result


@pytest.mark.asyncio
async def test_dns_probe_renders_addresses(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake(*args: Any, **kwargs: Any) -> list[Any]:
        return [(socket.AF_INET, socket.SOCK_STREAM, 0, "", ("10.0.0.5", 0))]

    monkeypatch.setattr("fleetfix.modules.network.dns.socket.getaddrinfo", fake)
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        app.query_one("#probe-target", Input).value = "svc.internal"
        await pilot.click("#probe-dns")
        await app.workers.wait_for_complete()
        await pilot.pause()
        result = str(app.query_one("#probe-result", Static).render())
        assert "10.0.0.5" in result


@pytest.mark.asyncio
async def test_ping_probe_renders_summary(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_ping(target: str, **_: Any) -> PingSummary:
        return PingSummary(
            target=target,
            sent=10,
            received=10,
            loss_pct=0.0,
            rtt_min_ms=1.0,
            rtt_avg_ms=2.5,
            rtt_max_ms=4.0,
            rtt_mdev_ms=0.5,
        )

    monkeypatch.setattr("fleetfix.screens.network.run_ping", fake_ping)
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        app.query_one("#probe-target", Input).value = "10.0.0.1"
        await pilot.click("#probe-ping")
        await app.workers.wait_for_complete()
        await pilot.pause()
        result = str(app.query_one("#probe-result", Static).render())
        assert "10/10" in result
        assert "avg 2.5ms" in result


@pytest.mark.asyncio
async def test_empty_target_shows_prompt() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        await pilot.click("#probe-curl")
        await pilot.pause()
        result = str(app.query_one("#probe-result", Static).render())
        assert "Enter a target" in result


@pytest.mark.asyncio
async def test_superseded_probe_result_never_lands(monkeypatch: pytest.MonkeyPatch) -> None:
    # exclusive=True cancels the Worker object but cannot interrupt a thread
    # already inside a subprocess call. The abandoned thread still reaches its
    # call_from_thread, and before the generation counter it clobbered the newer
    # result — a stale answer on screen with no sign anything was wrong.
    release = threading.Event()

    def slow_ping(target: str, **_: Any) -> PingSummary:
        release.wait(5)
        return PingSummary(
            target=target,
            sent=10,
            received=10,
            loss_pct=0.0,
            rtt_min_ms=1.0,
            rtt_avg_ms=99.9,
            rtt_max_ms=100.0,
            rtt_mdev_ms=0.5,
        )

    def fast_curl(url: str, **_: Any) -> CurlProbe:
        return CurlProbe(
            url=url,
            ok=True,
            http_code=204,
            time_total_s=0.01,
            time_namelookup_s=0.001,
            time_connect_s=0.002,
            time_appconnect_s=0.003,
            time_starttransfer_s=0.004,
            size_download_bytes=0,
        )

    monkeypatch.setattr("fleetfix.screens.network.run_ping", slow_ping)
    monkeypatch.setattr("fleetfix.screens.network.run_curl", fast_curl)
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        app.query_one("#probe-target", Input).value = "10.0.0.1"
        await pilot.click("#probe-ping")
        # Supersede it while the ping thread is still blocked.
        await pilot.click("#probe-curl")
        await pilot.pause()
        release.set()
        await app.workers.wait_for_complete()
        await pilot.pause()
        result = str(app.query_one("#probe-result", Static).render())
        verdict = str(app.query_one("#probe-verdict", Static).render())
        assert "HTTP 204" in verdict
        assert "HTTP 204" in result
        assert "99.9ms" not in result


@pytest.mark.asyncio
async def test_header_panels_populate_on_first_show(_stub_facts: None) -> None:
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        link = str(app.query_one("#link-summary", Static).render())
        dns = str(app.query_one("#resolver-summary", Static).render())
        assert "eth0" in link
        assert "10.0.0.9" in link
        assert "10.0.0.1" in link
        assert "up" in link
        assert "10.0.0.53" in dns
        assert "corp.internal" in dns


@pytest.mark.asyncio
async def test_no_default_route_says_so(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.screens.network.read_network", lambda: None)
    monkeypatch.setattr("fleetfix.screens.network.read_resolver", lambda: _resolver("10.0.0.53"))
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        assert "no default route" in str(app.query_one("#link-summary", Static).render())


@pytest.mark.asyncio
async def test_systemd_stub_resolver_points_at_resolvectl(monkeypatch: pytest.MonkeyPatch) -> None:
    # "nameserver 127.0.0.53" on its own sends an operator hunting a broken
    # loopback resolver; the real upstreams are behind the stub.
    monkeypatch.setattr("fleetfix.screens.network.read_network", lambda: _UP)
    monkeypatch.setattr("fleetfix.screens.network.read_resolver", lambda: _resolver("127.0.0.53"))
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        dns = str(app.query_one("#resolver-summary", Static).render())
        assert "resolvectl status" in dns


@pytest.mark.asyncio
async def test_refresh_reloads_facts_and_sockets(monkeypatch: pytest.MonkeyPatch) -> None:
    calls = {"net": 0, "resolver": 0, "sockets": 0}

    def count(key: str, value: Any) -> Any:
        calls[key] += 1
        return value

    monkeypatch.setattr("fleetfix.screens.network.read_network", lambda: count("net", _UP))
    monkeypatch.setattr(
        "fleetfix.screens.network.read_resolver", lambda: count("resolver", _resolver("10.0.0.53"))
    )
    monkeypatch.setattr(
        "fleetfix.screens.network.list_listening_sockets", lambda: count("sockets", [])
    )
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        assert calls == {"net": 1, "resolver": 1, "sockets": 1}
        # The screen had no refresh control at all before this.
        await pilot.click("#net-refresh")
        await app.workers.wait_for_complete()
        await pilot.pause()
        assert calls == {"net": 2, "resolver": 2, "sockets": 2}


def _hop(number: int, host: str | None, rtt: float | None) -> TraceHop:
    if host is None:
        return TraceHop(number=number, hosts=(), rtts_ms=(), timeouts=3)
    return TraceHop(number=number, hosts=(host,), rtts_ms=(rtt,) if rtt else (), timeouts=0)


async def _run_probe(app: FleetFixApp, pilot: Any, target: str, button: str) -> tuple[str, str]:
    """Type a target, click a probe button, return (verdict, raw pane) text."""
    await pilot.pause()
    app.action_switch("network")
    await app.workers.wait_for_complete()
    await pilot.pause()
    app.query_one("#probe-target", Input).value = target
    await pilot.click(button)
    await app.workers.wait_for_complete()
    await pilot.pause()
    return (
        str(app.query_one("#probe-verdict", Static).render()),
        str(app.query_one("#probe-result", Static).render()),
    )


@pytest.mark.asyncio
async def test_trace_that_reaches_the_target_passes(monkeypatch: pytest.MonkeyPatch) -> None:
    result = TraceResult(
        target="8.8.8.8",
        tool="traceroute",
        hops=(_hop(1, "10.0.0.1", 0.5), _hop(2, "203.0.113.9", 8.2), _hop(3, "8.8.8.8", 11.4)),
        reached=True,
        max_hops=15,
        raw="traceroute to 8.8.8.8 (8.8.8.8), 15 hops max\n 1  10.0.0.1  0.500 ms\n",
    )
    monkeypatch.setattr("fleetfix.screens.network.select_tool", lambda: "traceroute")
    monkeypatch.setattr("fleetfix.screens.network.run_trace", lambda *a, **k: result)
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        verdict, raw = await _run_probe(app, pilot, "8.8.8.8", "#probe-trace")
        assert "reached in 3 hops" in verdict
        assert app.query_one("#probe-verdict", Static).has_class("verdict-ok")
        assert "203.0.113.9" in raw
        # The tool's verbatim output is shown, not just our parse of it.
        assert "15 hops max" in raw


@pytest.mark.asyncio
async def test_trace_going_dark_is_a_warning_not_a_failure(monkeypatch: pytest.MonkeyPatch) -> None:
    # ICMP-rate-limited transit is extremely common and says nothing about whether
    # the destination is reachable — calling it a failure would cry wolf.
    result = TraceResult(
        target="8.8.8.8",
        tool="tracepath",
        hops=(_hop(1, "10.0.0.1", 0.5), _hop(2, "203.0.113.9", 8.2), _hop(3, None, None)),
        reached=False,
        max_hops=15,
        raw="1: 10.0.0.1  0.500ms\n3: no reply\n",
    )
    monkeypatch.setattr("fleetfix.screens.network.select_tool", lambda: "tracepath")
    monkeypatch.setattr("fleetfix.screens.network.run_trace", lambda *a, **k: result)
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        verdict, raw = await _run_probe(app, pilot, "8.8.8.8", "#probe-trace")
        assert "goes dark after hop 2" in verdict
        assert app.query_one("#probe-verdict", Static).has_class("verdict-warn")
        assert not app.query_one("#probe-verdict", Static).has_class("verdict-bad")
        # The dark hop is still listed, so the operator can see where it stops.
        assert "no reply" in raw


@pytest.mark.asyncio
async def test_missing_traceroute_binary_reports_how_to_install(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    result = TraceResult(
        target="8.8.8.8",
        tool="",
        hops=(),
        reached=False,
        max_hops=15,
        raw="",
        error="neither traceroute nor tracepath is installed — install one with: "
        "apt install traceroute (or: apt install iputils-tracepath)",
    )
    monkeypatch.setattr("fleetfix.screens.network.select_tool", lambda: "")
    monkeypatch.setattr("fleetfix.screens.network.run_trace", lambda *a, **k: result)
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        verdict, _raw = await _run_probe(app, pilot, "8.8.8.8", "#probe-trace")
        assert "apt install traceroute" in verdict


@pytest.mark.asyncio
async def test_open_port_passes(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "fleetfix.screens.network.check_port",
        lambda target, **_: TcpCheck(target=target, state="open", latency_ms=12.0),
    )
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        verdict, raw = await _run_probe(app, pilot, "db.internal:5432", "#probe-port")
        assert "db.internal:5432 — open" in verdict
        assert app.query_one("#probe-verdict", Static).has_class("verdict-ok")
        assert "accepted a TCP connection" in raw


@pytest.mark.asyncio
async def test_refused_port_is_a_warning_because_the_path_works(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(
        "fleetfix.screens.network.check_port",
        lambda target, **_: TcpCheck(target=target, state="refused", latency_ms=0.4),
    )
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        verdict, raw = await _run_probe(app, pilot, "127.0.0.1:1", "#probe-port")
        assert app.query_one("#probe-verdict", Static).has_class("verdict-warn")
        assert "refused" in verdict
        # Attribution is the point: refused means the layers under TCP are fine.
        assert "nothing is listening" in raw


@pytest.mark.asyncio
async def test_dropped_port_is_a_failure(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "fleetfix.screens.network.check_port",
        lambda target, **_: TcpCheck(target=target, state="timeout", latency_ms=3000.0),
    )
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        verdict, raw = await _run_probe(app, pilot, "192.0.2.1:5432", "#probe-port")
        assert app.query_one("#probe-verdict", Static).has_class("verdict-bad")
        assert "timeout" in verdict
        assert "firewall dropping packets" in raw


@pytest.mark.asyncio
async def test_port_check_refuses_to_guess_a_port(monkeypatch: pytest.MonkeyPatch) -> None:
    called: list[TcpTarget] = []

    def spy(target: TcpTarget, **_: Any) -> TcpCheck:
        called.append(target)
        return TcpCheck(target=target, state="open", latency_ms=1.0)

    monkeypatch.setattr("fleetfix.screens.network.check_port", spy)
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        verdict, raw = await _run_probe(app, pilot, "db.internal", "#probe-port")
        assert "explicit port" in verdict
        assert "host:port" in raw
        # Probing 80 when the operator meant 5432 answers a question nobody asked.
        assert called == []


# Every control on the Checks tab, in the order they compose.
_CONTROL_IDS = (
    "#quick-all",
    "#quick-gateway",
    "#quick-internet",
    "#quick-set",
    "#net-refresh",
    "#probe-target",
    "#probe-curl",
    "#probe-dns",
    "#probe-ping",
    "#probe-trace",
    "#probe-port",
)


@pytest.mark.asyncio
async def test_controls_reachable_at_80x24() -> None:
    # 80x24 is the floor we support — a serial console. The failure mode this
    # catches is a button that composes but gets clipped to zero width, which
    # makes it silently unclickable rather than visibly broken.
    app = FleetFixApp()
    async with app.run_test(size=(80, 24)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        for control_id in _CONTROL_IDS:
            region = app.query_one(control_id).region
            assert region.width > 0, f"{control_id} is clipped to zero width at 80x24"
            assert region.height > 0, f"{control_id} is clipped to zero height at 80x24"
        # The two extremes of the layout: first quick check, last probe button.
        await pilot.click("#net-refresh")
        await pilot.click("#probe-port")
        await pilot.pause()
        # The raw pane still gets usable room after every control is placed.
        assert app.query_one("#raw-pane").region.height >= 4


@pytest.mark.asyncio
async def test_sockets_table_lives_in_its_own_tab() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(80, 24)) as pilot:
        await pilot.pause()
        app.action_switch("network")
        await app.workers.wait_for_complete()
        await pilot.pause()
        # Checks is what an operator lands on.
        assert app.query_one("#net-tabs", TabbedContent).active == "tab-checks"
        table = app.query_one("#sockets-table", DataTable)
        # It loaded eagerly despite its pane being inactive, so switching to it
        # is instant.
        assert table.row_count == 2
        assert table.query_ancestor(TabPane).id == "tab-sockets"
