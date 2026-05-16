"""End-to-end Textual pilot tests for the app shell."""

from __future__ import annotations

from pathlib import Path

import pytest

from fleetfix.app import FleetFixApp
from fleetfix.screens.dashboard import DashboardView, MetricCard
from fleetfix.screens.services import ServicesView
from fleetfix.widgets.nav import Nav
from fleetfix.widgets.topbar import TopBar


@pytest.fixture(autouse=True)
def _audit_in_tmp(tmp_path: Path, monkeypatch: pytest.MonkeyPatch) -> None:
    """Redirect resolve_audit_path() so tests don't fight /var/log permissions."""
    audit_file = tmp_path / "audit.log"
    monkeypatch.setattr("fleetfix.app.resolve_audit_path", lambda: audit_file)


@pytest.mark.asyncio
async def test_app_mounts_with_all_components() -> None:
    app = FleetFixApp()
    async with app.run_test() as pilot:
        await pilot.pause()
        assert app.query_one(TopBar)
        assert app.query_one(Nav)
        assert app.query_one(DashboardView)
        cards = app.query(MetricCard)
        assert len(list(cards)) == 5


@pytest.mark.asyncio
async def test_dashboard_renders_metric_values() -> None:
    app = FleetFixApp()
    async with app.run_test() as pilot:
        await pilot.pause()
        await pilot.pause()  # let on_mount refresh complete
        uptime_card = app.query_one("#card-uptime", MetricCard)
        value = uptime_card.query(".metric-value").first()
        assert value is not None
        rendered = str(value.render())
        assert rendered != "—"
        assert rendered != ""


@pytest.mark.asyncio
async def test_switching_views_works(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.screens.services.list_failed_units", lambda: [])
    monkeypatch.setattr("fleetfix.screens.services.blame", lambda: [])
    app = FleetFixApp()
    async with app.run_test() as pilot:
        await pilot.pause()
        app.action_switch("services")
        await pilot.pause()
        from textual.widgets import ContentSwitcher

        switcher = app.query_one("#content", ContentSwitcher)
        assert switcher.current == "view-services"
        visible = app.query_one("#view-services", ServicesView)
        assert visible is not None


@pytest.mark.asyncio
async def test_read_only_mode_shows_banner() -> None:
    app = FleetFixApp(read_only=True)
    async with app.run_test() as pilot:
        await pilot.pause()
        topbar = app.query_one(TopBar)
        assert topbar.read_only is True
