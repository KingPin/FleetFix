"""Pilot tests for the Storage view's stale finder + env check wiring."""

from __future__ import annotations

import os
import time
from pathlib import Path

import pytest
from textual.widgets import Button, DataTable, Input, Static

from fleetfix.app import FleetFixApp
from fleetfix.config import InspectTarget


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


def _seed_stale(root: Path) -> None:
    root.mkdir(parents=True, exist_ok=True)
    target = root / "huge.sql.gz"
    target.write_bytes(b"x" * 4096)
    past = time.time() - 90 * 86400
    os.utime(target, (past, past))


@pytest.mark.asyncio
async def test_storage_scan_populates_table(tmp_path: Path) -> None:
    scan_root = tmp_path / "home"
    _seed_stale(scan_root)

    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("storage")
        await pilot.pause()

        root_input = app.query_one("#stale-root", Input)
        root_input.value = str(scan_root)
        days_input = app.query_one("#stale-days", Input)
        days_input.value = "30"
        await pilot.click("#stale-scan")
        # The scan now runs in a thread worker so the TUI stays responsive;
        # wait for it to finish before asserting on the populated table.
        await app.workers.wait_for_complete()
        await pilot.pause()

        table = app.query_one("#stale-table", DataTable)
        assert table.row_count == 1
        scan_btn = app.query_one("#stale-scan", Button)
        assert scan_btn.disabled is False
        delete_btn = app.query_one("#stale-delete", Button)
        assert delete_btn.disabled is False


@pytest.mark.asyncio
async def test_env_check_reports_ok_for_valid_file(tmp_path: Path) -> None:
    env = tmp_path / "good.env"
    env.write_text("DB_URL=postgres://localhost\nAPI_KEY=secret\n")

    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("storage")
        await pilot.pause()
        input_widget = app.query_one("#env-input", Input)
        input_widget.value = str(env)
        await pilot.click("#env-check")
        await pilot.pause()
        result = app.query_one("#env-result", Static)
        rendered = str(result.render())
        assert "ok" in rendered
        assert str(env) in rendered


@pytest.mark.asyncio
async def test_env_check_flags_missing_file(tmp_path: Path) -> None:
    app = FleetFixApp()
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("storage")
        await pilot.pause()
        input_widget = app.query_one("#env-input", Input)
        input_widget.value = str(tmp_path / "nope.env")
        await pilot.click("#env-check")
        await pilot.pause()
        result = app.query_one("#env-result", Static)
        assert "does not exist" in str(result.render())


@pytest.mark.asyncio
async def test_storage_uses_inspect_target_home(tmp_path: Path) -> None:
    fake_home = tmp_path / "fake_home"
    fake_home.mkdir()

    app = FleetFixApp()
    app.inspect_target = InspectTarget(user="targetuser", home=fake_home, uid=4242)
    async with app.run_test(size=(160, 60)) as pilot:
        await pilot.pause()
        app.action_switch("storage")
        await pilot.pause()

        stale_root_input = app.query_one("#stale-root", Input)
        env_input = app.query_one("#env-input", Input)
        assert stale_root_input.value == str(fake_home)
        assert env_input.value == str(fake_home / ".env")
