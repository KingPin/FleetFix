"""Pilot tests for the Docker view — subprocess + audit calls stubbed."""

from __future__ import annotations

from datetime import datetime, timezone
from pathlib import Path

import pytest
from textual.widgets import DataTable, Input, Static

from fleetfix.app import FleetFixApp
from fleetfix.modules.docker.dashboard import Container
from fleetfix.modules.docker.hygiene import DfRow, PruneResult
from fleetfix.modules.docker.truncate import TruncateResult


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: tmp_path / "audit.log")


@pytest.fixture(autouse=True)
def _stub_docker(monkeypatch: pytest.MonkeyPatch) -> None:
    now = datetime.now(timezone.utc)
    monkeypatch.setattr(
        "fleetfix.screens.docker.list_containers",
        lambda: [
            Container(
                id="abc123",
                name="api",
                image="myapp:1.0",
                state="running",
                status="Up 5 minutes",
                ports="80/tcp",
                restart_count=0,
                started_at=now,
                log_path="/var/lib/docker/containers/abc123/abc123-json.log",
            ),
            Container(
                id="def456",
                name="flappy",
                image="busted:latest",
                state="running",
                status="Restarting (1) 3 seconds ago",
                ports="",
                restart_count=10,
                started_at=now,
                log_path="/var/lib/docker/containers/def456/def456-json.log",
            ),
        ],
    )
    monkeypatch.setattr(
        "fleetfix.screens.docker.system_df",
        lambda: [
            DfRow(
                type="Images",
                total_count=10,
                active=5,
                size_bytes=10 * 1000**3,
                reclaimable_bytes=5 * 1000**3,
                reclaimable_pct=50,
            ),
            DfRow(
                type="Containers",
                total_count=2,
                active=2,
                size_bytes=1000,
                reclaimable_bytes=0,
                reclaimable_pct=0,
            ),
        ],
    )


@pytest.mark.asyncio
async def test_docker_view_populates_containers_and_df() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("docker")
        await app.workers.wait_for_complete()
        await pilot.pause()
        table = app.query_one("#docker-table", DataTable)
        df = app.query_one("#docker-df-table", DataTable)
        assert table.row_count == 2
        assert df.row_count == 2


@pytest.mark.asyncio
async def test_docker_view_flags_restart_loop() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("docker")
        await app.workers.wait_for_complete()
        await pilot.pause()
        summary = str(app.query_one("#docker-summary", Static).render())
        assert "1 in restart loop" in summary


@pytest.mark.asyncio
async def test_truncate_button_pushes_confirm_modal(monkeypatch: pytest.MonkeyPatch) -> None:
    called: dict[str, object] = {}

    def fake_truncate(container_id, *, audit, container_name=""):  # type: ignore[no-untyped-def]
        called["id"] = container_id
        called["name"] = container_name
        return TruncateResult(
            container_id=container_id,
            log_path="/var/lib/docker/containers/abc123/abc123-json.log",
            bytes_freed=42_000,
            ok=True,
        )

    monkeypatch.setattr("fleetfix.screens.docker.truncate_log", fake_truncate)

    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("docker")
        await app.workers.wait_for_complete()
        await pilot.pause()
        await pilot.click("#docker-truncate")
        await pilot.pause()
        inp = app.screen.query_one("#confirm-input", Input)
        inp.value = "TRUNCATE"
        await pilot.pause()
        await pilot.click("#confirm-submit")
        await pilot.pause()
        assert called["id"] == "abc123"
        assert called["name"] == "api"
        msg = str(app.query_one("#docker-result", Static).render())
        assert "Truncated" in msg


@pytest.mark.asyncio
async def test_prune_images_button_pushes_confirm_modal(
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    def fake_prune(*, audit, **kwargs):  # type: ignore[no-untyped-def]
        return PruneResult(target="images", bytes_reclaimed=500 * 1000**2, ok=True)

    monkeypatch.setattr("fleetfix.screens.docker.prune_images", fake_prune)

    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("docker")
        await app.workers.wait_for_complete()
        await pilot.pause()
        await pilot.click("#docker-prune-images")
        await pilot.pause()
        inp = app.screen.query_one("#confirm-input", Input)
        inp.value = "PRUNE"
        await pilot.pause()
        await pilot.click("#confirm-submit")
        await pilot.pause()
        msg = str(app.query_one("#docker-result", Static).render())
        assert "Pruned images" in msg


@pytest.mark.asyncio
async def test_truncate_without_selection_prompts() -> None:
    app = FleetFixApp()
    async with app.run_test(size=(200, 60)) as pilot:
        await pilot.pause()
        app.action_switch("docker")
        await app.workers.wait_for_complete()
        await pilot.pause()
        table = app.query_one("#docker-table", DataTable)
        table.clear()
        await pilot.click("#docker-truncate")
        await pilot.pause()
        msg = str(app.query_one("#docker-result", Static).render())
        assert "Select a container" in msg
