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
from fleetfix.modules.network.ping import PingSummary
from fleetfix.modules.network.sockets import ListeningSocket


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
