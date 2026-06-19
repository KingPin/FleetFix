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
        assert len(list(cards)) == 9


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
async def test_dashboard_slow_tier_cards_populate(monkeypatch: pytest.MonkeyPatch) -> None:
    """The thread-worker slow tier must render storage/network/services end-to-end."""
    from fleetfix.modules.disk import usage
    from fleetfix.modules.network.interfaces import NetworkInfo
    from fleetfix.screens import dashboard

    monkeypatch.setattr(
        dashboard.usage,
        "run_df",
        lambda **_k: [usage.DiskUsage("/dev/sda1", "/", 100, 91, 9, 91)],
    )
    monkeypatch.setattr(dashboard.inodes, "run_df_inodes", lambda **_k: [])
    failed_calls: list[str | None] = []

    def _fake_failed(target_user: str | None = None) -> list:
        failed_calls.append(target_user)
        return []

    monkeypatch.setattr(dashboard.failed, "list_failed_units", _fake_failed)
    monkeypatch.setattr(
        dashboard.interfaces,
        "read_network",
        lambda: NetworkInfo("eth0", "10.0.0.5", "10.0.0.1", "up", 1000, 2000),
    )

    # Disable the launch-time update worker so wait_for_complete() blocks only on
    # the dashboard's slow-tier worker, not a (potentially networked) release check.
    app = FleetFixApp(check_for_update_on_mount=False)
    async with app.run_test() as pilot:
        await pilot.pause()
        await app.workers.wait_for_complete()  # let the slow-tier worker finish
        await pilot.pause()

        def _value(card_id: str) -> str:
            card = app.query_one(card_id, MetricCard)
            return str(card.query(".metric-value").first().render())

        assert "91%" in _value("#card-storage")
        assert "eth0" in _value("#card-network")
        assert _value("#card-services") == "none failed"
        # The card filters by inspect target like ServicesView (None here).
        assert failed_calls == [None]


@pytest.mark.asyncio
async def test_switching_views_works(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("fleetfix.screens.services.list_failed_units", lambda **_kwargs: [])
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
