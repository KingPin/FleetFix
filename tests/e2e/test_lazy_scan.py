"""Lazy per-view scans + dashboard refresh gating (issue #2).

Guards the behavior that fixed the launch-time thundering herd: only the
initial view scans at launch, other views scan on first show/switch, and the
dashboard's periodic refresh pauses while it isn't the visible pane.
"""

from __future__ import annotations

from pathlib import Path

import pytest

from fleetfix.app import FleetFixApp
from fleetfix.screens.audit_log import AuditLogView
from fleetfix.screens.dashboard import DashboardView
from fleetfix.screens.disk import DiskView
from fleetfix.screens.processes import ProcessesView


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


@pytest.fixture(autouse=True)
def _fast_procs(monkeypatch: pytest.MonkeyPatch) -> None:
    # Keep the processes scan trivial so switching to it stays hermetic.
    monkeypatch.setattr("fleetfix.screens.processes.snapshot", lambda **_: [])


@pytest.mark.asyncio
async def test_only_initial_view_scans_at_launch() -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        # The initial pane (dashboard) is shown, so it loaded...
        assert app.query_one(DashboardView)._initial_scan_done is True
        # ...but nothing else did — no thundering herd of launch scans.
        assert app.query_one(ProcessesView)._initial_scan_done is False
        assert app.query_one(DiskView)._initial_scan_done is False


@pytest.mark.asyncio
async def test_switching_loads_view_once() -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        procs = app.query_one(ProcessesView)
        assert procs._initial_scan_done is False

        app.action_switch("processes")
        assert procs._initial_scan_done is True  # triggered synchronously on switch

        # Switching away and back must not rescan (revisits stay instant).
        app.action_switch("dashboard")
        await pilot.pause()
        app.action_switch("processes")
        await pilot.pause()
        assert procs._initial_scan_done is True


@pytest.mark.asyncio
async def test_dashboard_refresh_pauses_while_hidden() -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        dash = app.query_one(DashboardView)
        assert dash._slow_timer is not None
        # Running while visible.
        assert dash._slow_timer._active.is_set() is True
        assert dash._fast_timer is not None and dash._fast_timer._active.is_set() is True

        # Navigate away → both refresh tiers pause.
        app.action_switch("processes")
        await pilot.pause()
        assert dash._slow_timer._active.is_set() is False
        assert dash._fast_timer._active.is_set() is False

        # Return → they resume.
        app.action_switch("dashboard")
        await pilot.pause()
        assert dash._slow_timer._active.is_set() is True
        assert dash._fast_timer._active.is_set() is True


@pytest.mark.asyncio
async def test_audit_log_tail_pauses_while_hidden() -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        audit = app.query_one(AuditLogView)
        # Not the initial pane → no tail timer until it is first shown.
        assert audit._initial_scan_done is False
        assert audit._timer is None

        app.action_switch("audit")
        await pilot.pause()
        assert audit._initial_scan_done is True
        assert audit._timer is not None and audit._timer._active.is_set() is True

        app.action_switch("dashboard")
        await pilot.pause()
        assert audit._timer._active.is_set() is False  # paused while hidden

        app.action_switch("audit")
        await pilot.pause()
        assert audit._timer._active.is_set() is True  # resumed on return
