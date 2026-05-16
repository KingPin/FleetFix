"""Dashboard view — live load, memory, uptime, thermal.

Despite the package name `screens/`, this is a regular `Widget` because the
app uses a `ContentSwitcher` to swap the main content area while keeping the
top bar and nav visible. Future views (storage, network, etc.) follow the
same pattern.
"""

from __future__ import annotations

import os

from textual.app import ComposeResult
from textual.containers import Grid
from textual.widget import Widget
from textual.widgets import Static

from fleetfix.modules.system import metrics, thermal, updates


class MetricCard(Widget):
    """One panel in the dashboard grid: title on top, value(s) below."""

    DEFAULT_CSS = """
    MetricCard {
        border: round $primary-darken-1;
        padding: 1 2;
        height: auto;
        min-height: 5;
    }
    MetricCard .metric-title {
        color: $accent;
        text-style: bold;
    }
    MetricCard .metric-value {
        color: $text;
        padding-top: 1;
    }
    MetricCard .metric-warn {
        color: $warning;
    }
    MetricCard .metric-critical {
        color: $error;
    }
    """

    def __init__(self, title: str, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._title = title

    def compose(self) -> ComposeResult:
        yield Static(self._title, classes="metric-title")
        yield Static("—", classes="metric-value")

    def set_value(self, value: str, *, severity: str = "") -> None:
        label = self.query_one(".metric-value", Static)
        label.update(value)
        classes = "metric-value"
        if severity:
            classes = f"{classes} {severity}"
        label.set_classes(classes)


def _load_severity(load_one: float, cpu_count: int) -> str:
    ratio = load_one / max(cpu_count, 1)
    if ratio >= 2.0:
        return "metric-critical"
    if ratio >= 1.0:
        return "metric-warn"
    return ""


def _mem_severity(used_pct: float) -> str:
    if used_pct >= 95.0:
        return "metric-critical"
    if used_pct >= 80.0:
        return "metric-warn"
    return ""


def _temp_severity(temp_c: float) -> str:
    if temp_c >= 85.0:
        return "metric-critical"
    if temp_c >= 70.0:
        return "metric-warn"
    return ""


def _updates_severity(status: updates.UpdateStatus) -> str:
    if status.security >= 5:
        return "metric-critical"
    if status.security > 0:
        return "metric-warn"
    return ""


class DashboardView(Widget):
    DEFAULT_CSS = """
    DashboardView {
        height: 1fr;
    }
    DashboardView Grid {
        grid-size: 3 2;
        grid-gutter: 1 2;
        padding: 1 2;
        height: 1fr;
    }
    """

    REFRESH_INTERVAL = 2.0  # seconds

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._cpu_count = os.cpu_count() or 1

    def compose(self) -> ComposeResult:
        with Grid():
            yield MetricCard("Uptime", id="card-uptime")
            yield MetricCard("Load avg (1 / 5 / 15)", id="card-load")
            yield MetricCard("Memory", id="card-mem")
            yield MetricCard("Thermal", id="card-thermal")
            yield MetricCard("Pending updates", id="card-updates")

    def on_mount(self) -> None:
        self.refresh_metrics()
        self.set_interval(self.REFRESH_INTERVAL, self.refresh_metrics)

    def refresh_metrics(self) -> None:
        try:
            snapshot = metrics.read_all()
        except OSError as exc:
            self._set("card-uptime", f"unavailable: {exc}", "metric-critical")
            return

        self._set("card-uptime", metrics.format_uptime(snapshot.uptime_seconds))
        load = snapshot.load
        cpu_label = "CPU" if self._cpu_count == 1 else "CPUs"
        self._set(
            "card-load",
            f"{load.one:.2f}  {load.five:.2f}  {load.fifteen:.2f}    "
            f"({self._cpu_count} {cpu_label})",
            _load_severity(load.one, self._cpu_count),
        )

        mem = snapshot.memory
        gb_used = mem.used_kb / 1024 / 1024
        gb_total = mem.total_kb / 1024 / 1024
        line = f"{gb_used:.1f} / {gb_total:.1f} GB  ({mem.used_pct:.0f}% used)"
        if mem.swap_total_kb:
            swap_gb_used = mem.swap_used_kb / 1024 / 1024
            swap_gb_total = mem.swap_total_kb / 1024 / 1024
            line += f"\nSwap: {swap_gb_used:.1f} / {swap_gb_total:.1f} GB"
        self._set("card-mem", line, _mem_severity(mem.used_pct))

        hottest = thermal.hottest()
        if hottest is None:
            self._set("card-thermal", "no thermal sensors")
        else:
            self._set(
                "card-thermal",
                f"{hottest.type}: {hottest.temp_c:.1f}°C",
                _temp_severity(hottest.temp_c),
            )

        update_status = updates.get_update_status()
        if update_status.source == "unavailable":
            self._set("card-updates", "unavailable")
        elif update_status.upgradable == 0:
            self._set("card-updates", "up to date")
        else:
            line = f"{update_status.upgradable} upgradable"
            if update_status.security:
                line += f"  ({update_status.security} security)"
            line += f"\nsource: {update_status.source}"
            self._set("card-updates", line, _updates_severity(update_status))

    def _set(self, card_id: str, value: str, severity: str = "") -> None:
        card = self.query_one(f"#{card_id}", MetricCard)
        card.set_value(value, severity=severity)
