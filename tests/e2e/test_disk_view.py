"""Pilot tests for the Disk view — all subprocess calls stubbed."""

from __future__ import annotations

from pathlib import Path

import pytest
from textual.widgets import DataTable, Static

from fleetfix.app import FleetFixApp
from fleetfix.modules.disk.ghost import GhostFile
from fleetfix.modules.disk.inodes import InodeUsage
from fleetfix.modules.disk.smart import SmartReport


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


@pytest.fixture(autouse=True)
def _stub_disk_modules(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(
        "fleetfix.screens.disk.report_all",
        lambda: [
            SmartReport(
                device="/dev/sda",
                kind="sata",
                health="PASSED",
                attributes={"reallocated_sectors": 0, "power_on_hours": 1000},
            ),
            SmartReport(
                device="/dev/nvme0n1",
                kind="nvme",
                health="PASSED",
                attributes={"percentage_used": 3, "available_spare": 100},
            ),
        ],
    )
    monkeypatch.setattr(
        "fleetfix.screens.disk.list_ghost_files",
        lambda: [
            GhostFile(
                pid=1234,
                command="postgres",
                user="postgres",
                fd="9",
                size_bytes=100_000_000,
                path="/var/lib/postgresql/14/main/pg_log.deleted",
            ),
        ],
    )
    monkeypatch.setattr(
        "fleetfix.screens.disk.run_df_inodes",
        lambda: [
            InodeUsage(
                filesystem="/dev/sda1", mount="/", total=1000, used=500, free=500, used_pct=50
            ),
            InodeUsage(
                filesystem="/dev/sdb1", mount="/var", total=1000, used=950, free=50, used_pct=95
            ),
        ],
    )


@pytest.mark.asyncio
async def test_disk_view_populates_all_three_tables() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("disk")
        await app.workers.wait_for_complete()
        await pilot.pause()
        smart = app.query_one("#smart-table", DataTable)
        ghost = app.query_one("#ghost-table", DataTable)
        inode = app.query_one("#inode-table", DataTable)
        assert smart.row_count == 2
        assert ghost.row_count == 1
        assert inode.row_count == 2


@pytest.mark.asyncio
async def test_smart_summary_counts_failures() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("disk")
        await app.workers.wait_for_complete()
        await pilot.pause()
        summary = str(app.query_one("#smart-summary", Static).render())
        assert "2 device" in summary
        assert "0 not PASSED" in summary


@pytest.mark.asyncio
async def test_inode_summary_counts_warnings() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("disk")
        await app.workers.wait_for_complete()
        await pilot.pause()
        summary = str(app.query_one("#inode-summary", Static).render())
        # /var at 95% counts as above the 85% warn threshold.
        assert "1 above 85%" in summary


@pytest.mark.asyncio
async def test_ghost_summary_shows_reclaimable() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("disk")
        await app.workers.wait_for_complete()
        await pilot.pause()
        summary = str(app.query_one("#ghost-summary", Static).render())
        # 100M comes out as "95.4 MB" with 1024 base. Just check the unit.
        assert "MB" in summary or "GB" in summary
        assert "reclaimable" in summary
