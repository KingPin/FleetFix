"""Pilot tests for the TopBar's 'Inspecting:' chip."""

from __future__ import annotations

from pathlib import Path

import pytest
from textual.widgets import Static

from fleetfix.app import FleetFixApp
from fleetfix.config import InspectTarget
from fleetfix.widgets.topbar import TopBar


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


def _render_text(topbar: TopBar) -> str:
    return " ".join(str(s.render()) for s in topbar.query(Static))


@pytest.mark.asyncio
async def test_topbar_omits_chip_when_no_target() -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        topbar = app.query_one(TopBar)
        assert "Inspecting:" not in _render_text(topbar)


@pytest.mark.asyncio
async def test_topbar_shows_chip_for_target(tmp_path: Path) -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    app.inspect_target = InspectTarget(user="appuser", home=tmp_path, uid=4242)
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        topbar = app.query_one(TopBar)
        assert "Inspecting: appuser" in _render_text(topbar)
