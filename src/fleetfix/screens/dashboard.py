"""Dashboard view — live load, memory, uptime, thermal, storage, network, services.

Despite the package name `screens/`, this is a regular `Widget` because the
app uses a `ContentSwitcher` to swap the main content area while keeping the
top bar and nav visible. Future views (storage, network, etc.) follow the
same pattern.

Refresh is split into two tiers so the event loop never stalls:

  * `refresh_fast` (2s) — pure /proc and /sys reads (uptime, load, memory,
    thermal, network). Cheap enough to run inline on the UI thread.
  * `refresh_slow` (15s) — a thread worker for the subprocess-backed cards
    (df, df -i, systemctl, apt). These would jank the 2s tick if run inline,
    the same reason the stale scan moved off-thread.
"""

from __future__ import annotations

import os
import time

from textual import work
from textual.app import ComposeResult
from textual.containers import Grid, VerticalScroll
from textual.widget import Widget
from textual.widgets import Static

from fleetfix.modules.disk import inodes, usage
from fleetfix.modules.network import interfaces
from fleetfix.modules.services import failed
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


def _disk_severity(used_pct: int) -> str:
    if used_pct >= usage.CRITICAL_PCT:
        return "metric-critical"
    if used_pct >= usage.WARN_PCT:
        return "metric-warn"
    return ""


def _inode_severity(used_pct: int) -> str:
    if used_pct >= inodes.CRITICAL_PCT:
        return "metric-critical"
    if used_pct >= inodes.WARN_PCT:
        return "metric-warn"
    return ""


def _services_severity(count: int) -> str:
    if count >= 3:
        return "metric-critical"
    if count > 0:
        return "metric-warn"
    return ""


def _human_rate(bytes_per_sec: float) -> str:
    """Compact throughput, e.g. 0B/s / 12.4K/s / 3.1M/s."""
    value = float(bytes_per_sec)
    for unit in ("B", "K", "M", "G"):
        if value < 1024 or unit == "G":
            return f"{value:.0f}{unit}/s" if unit == "B" else f"{value:.1f}{unit}/s"
        value /= 1024
    return f"{value:.1f}T/s"


class DashboardView(Widget):
    DEFAULT_CSS = """
    DashboardView {
        height: 1fr;
    }
    DashboardView Grid {
        grid-size: 3 3;
        grid-gutter: 1 2;
        padding: 1 2;
        height: auto;
    }
    """

    REFRESH_INTERVAL = 2.0  # seconds — fast tier (pure /proc + /sys reads)
    SLOW_INTERVAL = 15.0  # seconds — slow tier (subprocess-backed cards)

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._cpu_count = os.cpu_count() or 1
        # Previous network counters + timestamp, for throughput deltas.
        self._prev_net: tuple[int, int] | None = None
        self._prev_net_t: float | None = None

    def compose(self) -> ComposeResult:
        # VerticalScroll keeps the 3x3 grid usable on short terminals where the
        # cards would otherwise clip instead of scroll.
        with VerticalScroll(), Grid():
            yield MetricCard("Uptime", id="card-uptime")
            yield MetricCard("Load avg (1 / 5 / 15)", id="card-load")
            yield MetricCard("Memory", id="card-mem")
            yield MetricCard("Thermal", id="card-thermal")
            yield MetricCard("Storage", id="card-storage")
            yield MetricCard("Inodes", id="card-inodes")
            yield MetricCard("Network", id="card-network")
            yield MetricCard("Failed services", id="card-services")
            yield MetricCard("Pending updates", id="card-updates")

    def on_mount(self) -> None:
        self.refresh_fast()
        self.refresh_slow()
        self.set_interval(self.REFRESH_INTERVAL, self.refresh_fast)
        self.set_interval(self.SLOW_INTERVAL, self.refresh_slow)

    # --- Fast tier: pure file reads, safe inline on the UI thread ----------

    def refresh_fast(self) -> None:
        try:
            snapshot = metrics.read_all()
        except OSError as exc:
            self._set("card-uptime", f"unavailable: {exc}", "metric-critical")
        else:
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

        self._refresh_network()

    def _refresh_network(self) -> None:
        info = interfaces.read_network()
        if info is None:
            self._set("card-network", "no default route")
            self._prev_net = None
            self._prev_net_t = None
            return

        now = time.monotonic()
        rate_line = ""
        if self._prev_net is not None and self._prev_net_t is not None:
            dt = now - self._prev_net_t
            if dt > 0:
                rx_rate = max(info.rx_bytes - self._prev_net[0], 0) / dt
                tx_rate = max(info.tx_bytes - self._prev_net[1], 0) / dt
                rate_line = f"↓ {_human_rate(rx_rate)}   ↑ {_human_rate(tx_rate)}"
        self._prev_net = (info.rx_bytes, info.tx_bytes)
        self._prev_net_t = now

        ip = info.ipv4 or "no IPv4"
        line = f"{info.iface} ({info.operstate})  {ip}\ngw {info.gateway}"
        if rate_line:
            line += f"\n{rate_line}"
        severity = "metric-warn" if info.operstate == "down" else ""
        self._set("card-network", line, severity)

    # --- Slow tier: subprocess-backed cards, off the event loop ------------

    @work(thread=True, exclusive=True, group="dash-slow")
    def refresh_slow(self) -> None:
        """Run the subprocess-backed reads off the UI thread, then apply results.

        Each reader already swallows its own errors and returns an empty result
        on failure, so one missing tool never strands the others' cards.
        """
        disk_rows = usage.run_df()
        inode_rows = inodes.run_df_inodes()
        failed_units = failed.list_failed_units()
        try:
            update_status: updates.UpdateStatus | None = updates.get_update_status()
        except Exception:  # updater fallbacks are best-effort; never strand the worker
            update_status = None
        self.app.call_from_thread(
            self._apply_slow, disk_rows, inode_rows, failed_units, update_status
        )

    def _apply_slow(
        self,
        disk_rows: list[usage.DiskUsage],
        inode_rows: list[inodes.InodeUsage],
        failed_units: list[failed.FailedUnit],
        update_status: updates.UpdateStatus | None,
    ) -> None:
        """Back on the UI thread: render the slow-tier cards."""
        top_disk = usage.fullest(disk_rows)
        if top_disk is None:
            self._set("card-storage", "unavailable")
        else:
            used_gb = top_disk.used_kb / 1024 / 1024
            total_gb = top_disk.total_kb / 1024 / 1024
            self._set(
                "card-storage",
                f"{top_disk.mount}\n{used_gb:.1f} / {total_gb:.1f} GB  ({top_disk.used_pct}%)",
                _disk_severity(top_disk.used_pct),
            )

        top_inode = max(inode_rows, key=lambda r: r.used_pct, default=None)
        if top_inode is None:
            self._set("card-inodes", "ok")
        else:
            self._set(
                "card-inodes",
                f"{top_inode.mount}  {top_inode.used_pct}%",
                _inode_severity(top_inode.used_pct),
            )

        if not failed_units:
            self._set("card-services", "none failed")
        else:
            names = ", ".join(u.name for u in failed_units[:2])
            if len(failed_units) > 2:
                names += ", …"
            self._set(
                "card-services",
                f"{len(failed_units)} failed\n{names}",
                _services_severity(len(failed_units)),
            )

        if update_status is None or update_status.source == "unavailable":
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
