"""Pilot tests for the Network view — all subprocess + socket calls are stubbed."""

from __future__ import annotations

import socket
from pathlib import Path
from typing import Any

import pytest
from textual.widgets import DataTable, Input, Static

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
        await pilot.pause()
        app.query_one("#probe-target", Input).value = "https://api.internal/health"
        await pilot.click("#probe-curl")
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
        await pilot.pause()
        app.query_one("#probe-target", Input).value = "svc.internal"
        await pilot.click("#probe-dns")
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
        await pilot.pause()
        app.query_one("#probe-target", Input).value = "10.0.0.1"
        await pilot.click("#probe-ping")
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
        await pilot.pause()
        await pilot.click("#probe-curl")
        await pilot.pause()
        result = str(app.query_one("#probe-result", Static).render())
        assert "Enter a target" in result
