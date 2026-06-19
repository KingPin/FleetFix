"""Pilot tests for the Processes view — subprocess + os.kill stubbed."""

from __future__ import annotations

import signal
from pathlib import Path
from typing import Any

import pytest
from textual.widgets import DataTable, Input, Static

from fleetfix.app import FleetFixApp
from fleetfix.modules.procs.ranker import ProcInfo


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


@pytest.fixture(autouse=True)
def _fake_snapshot(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake(**_: Any) -> list[ProcInfo]:
        return [
            ProcInfo(
                pid=1234,
                comm="postgres",
                user="postgres",
                rss_bytes=500 * 1024 * 1024,
                cpu_pct=2.5,
                cmdline="postgres: writer process",
            ),
            ProcInfo(
                pid=5678,
                comm="python",
                user="kingpin",
                rss_bytes=100 * 1024 * 1024,
                cpu_pct=80.0,
                cmdline="/usr/bin/python -m mything",
            ),
        ]

    monkeypatch.setattr("fleetfix.screens.processes.snapshot", fake)


@pytest.mark.asyncio
async def test_processes_table_populates_on_mount() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("processes")
        await app.workers.wait_for_complete()
        await pilot.pause()
        table = app.query_one("#procs-table", DataTable)
        assert table.row_count == 2


@pytest.mark.asyncio
async def test_by_cpu_reorders() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("processes")
        await app.workers.wait_for_complete()
        await pilot.pause()
        await pilot.click("#procs-by-cpu")
        await pilot.pause()
        table = app.query_one("#procs-table", DataTable)
        # By CPU, python (80%) should come first.
        first_pid = str(table.get_cell_at((0, 0)))  # type: ignore[arg-type]
        assert first_pid == "5678"


@pytest.mark.asyncio
async def test_term_without_selection_prompts() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("processes")
        await app.workers.wait_for_complete()
        await pilot.pause()
        # Don't select anything; DataTable focuses row 0 on mount, but we
        # want to test "no selection" -> we'll clear the table by stubbing.
        table = app.query_one("#procs-table", DataTable)
        table.clear()
        await pilot.click("#procs-term")
        await pilot.pause()
        msg = str(app.query_one("#procs-result", Static).render())
        assert "Select a row" in msg


@pytest.mark.asyncio
async def test_term_selected_pushes_confirm_modal(monkeypatch: pytest.MonkeyPatch) -> None:
    sent: dict[str, int] = {}

    def fake_kill(pid: int, sig: int) -> None:
        sent["pid"] = pid
        sent["sig"] = sig

    import os

    monkeypatch.setattr(os, "kill", fake_kill)

    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("processes")
        await app.workers.wait_for_complete()
        await pilot.pause()
        # Cursor is on row 0 → pid 1234 (postgres) by default (RSS sort).
        await pilot.click("#procs-term")
        await pilot.pause()
        # Modal is up; type KILL.
        inp = app.screen.query_one("#confirm-input", Input)
        inp.value = "KILL"
        await pilot.pause()
        await pilot.click("#confirm-submit")
        await pilot.pause()
        assert sent == {"pid": 1234, "sig": signal.SIGTERM}
        msg = str(app.query_one("#procs-result", Static).render())
        assert "Sent SIGTERM" in msg
