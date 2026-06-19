"""Pilot tests for the in-app update action and its read-only gate."""

from __future__ import annotations

from pathlib import Path

import pytest

from fleetfix.app import FleetFixApp
from fleetfix.screens.confirm import ConfirmModal
from fleetfix.updater.checker import ReleaseInfo


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


def _release() -> ReleaseInfo:
    return ReleaseInfo(
        tag="v9.9.9",
        version="9.9.9",
        asset_url="https://x/asset",
        checksum_url="https://x/sums",
        html_url="https://x/release",
        body="notes",
    )


@pytest.mark.asyncio
async def test_read_only_blocks_update_action() -> None:
    app = FleetFixApp(read_only=True, check_for_update_on_mount=False)
    notes: list[tuple[str, str]] = []
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.update_release = _release()
        app.notify = lambda msg, **kw: notes.append((msg, kw.get("severity", "")))  # type: ignore[assignment,method-assign]
        app.action_show_update()
        await pilot.pause()
        assert not isinstance(app.screen, ConfirmModal)
    assert notes, "read-only mode should notify the operator"
    assert "read-only" in notes[0][0].lower()
    assert notes[0][1] == "warning"


@pytest.mark.asyncio
async def test_update_action_opens_confirm_when_writable() -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.update_release = _release()
        app.action_show_update()
        await pilot.pause()
        assert isinstance(app.screen, ConfirmModal)
