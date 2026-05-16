"""Pilot tests for the Services view — subprocess calls stubbed."""

from __future__ import annotations

from pathlib import Path

import pytest
from textual.widgets import DataTable, Static

from fleetfix.app import FleetFixApp
from fleetfix.modules.services.boot import BlameEntry
from fleetfix.modules.services.failed import FailedUnit


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


@pytest.fixture(autouse=True)
def _stub_services(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "fleetfix.screens.services.list_failed_units",
        lambda: [
            FailedUnit(
                name="kafka.service",
                load="loaded",
                active="failed",
                sub="failed",
                description="Apache Kafka",
            ),
            FailedUnit(
                name="redis.service",
                load="loaded",
                active="failed",
                sub="failed",
                description="Redis in-memory data store",
            ),
        ],
    )
    monkeypatch.setattr(
        "fleetfix.screens.services.blame",
        lambda: [
            BlameEntry(unit="archlinux-keyring-wkd-sync.service", duration_ms=59_647),
            BlameEntry(unit="NetworkManager-wait-online.service", duration_ms=5_569),
            BlameEntry(unit="NetworkManager.service", duration_ms=559),
        ],
    )


@pytest.mark.asyncio
async def test_services_view_populates_failed_and_blame() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("services")
        await pilot.pause()
        failed = app.query_one("#failed-table", DataTable)
        blame_table = app.query_one("#blame-table", DataTable)
        assert failed.row_count == 2
        assert blame_table.row_count == 3


@pytest.mark.asyncio
async def test_services_view_flags_blame_outliers() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("services")
        await pilot.pause()
        summary = str(app.query_one("#blame-summary", Static).render())
        assert "3 unit(s)" in summary
        assert "2 above 5s" in summary


@pytest.mark.asyncio
async def test_journal_tail_button_without_selection_prompts() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("services")
        await pilot.pause()
        table = app.query_one("#failed-table", DataTable)
        table.clear()
        await pilot.click("#services-journal")
        await pilot.pause()
        out = str(app.query_one("#journal-output", Static).render())
        assert "select" in out.lower()


@pytest.mark.asyncio
async def test_journal_tail_button_fetches_for_selected_unit(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    captured: dict[str, str] = {}

    def fake_tail(unit: str, **kwargs: object) -> str:
        captured["unit"] = unit
        return (
            "2026-05-16T10:00:00 kafka.service: started\n2026-05-16T10:01:00 kafka.service: died\n"
        )

    monkeypatch.setattr("fleetfix.screens.services.journal_tail", fake_tail)

    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("services")
        await pilot.pause()
        table = app.query_one("#failed-table", DataTable)
        table.cursor_coordinate = (0, 0)
        await pilot.pause()
        await pilot.click("#services-journal")
        await pilot.pause()
        assert captured["unit"] == "kafka.service"
        out = str(app.query_one("#journal-output", Static).render())
        assert "kafka.service: started" in out


@pytest.mark.asyncio
async def test_services_view_refresh_button_repopulates(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    calls = {"failed": 0, "blame": 0}

    def fake_failed() -> list[FailedUnit]:
        calls["failed"] += 1
        return []

    def fake_blame() -> list[BlameEntry]:
        calls["blame"] += 1
        return []

    monkeypatch.setattr("fleetfix.screens.services.list_failed_units", fake_failed)
    monkeypatch.setattr("fleetfix.screens.services.blame", fake_blame)

    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("services")
        await pilot.pause()
        baseline_failed = calls["failed"]
        baseline_blame = calls["blame"]
        await pilot.click("#services-refresh")
        await pilot.pause()
        assert calls["failed"] > baseline_failed
        assert calls["blame"] > baseline_blame
