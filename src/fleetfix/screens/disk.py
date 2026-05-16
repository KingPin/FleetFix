"""Tier 2 Disk screen — SMART health, ghost-space, inode pressure.

Three stacked panels, each refreshable independently. All three run a
sudo-backed command (`smartctl`, `lsof +L1`, `df -i`); on a Tier-1-only
box the table just stays empty rather than erroring loudly.
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widget import Widget
from textual.widgets import Button, DataTable, Static

from fleetfix.modules.disk.ghost import list_ghost_files, total_bytes
from fleetfix.modules.disk.inodes import run_df_inodes
from fleetfix.modules.disk.smart import report_all


class DiskView(Widget):
    DEFAULT_CSS = """
    DiskView {
        layout: vertical;
        height: 1fr;
        padding: 1 1 0 1;
    }
    DiskView .panel-title {
        text-style: bold;
        color: $accent;
        margin-top: 1;
        margin-bottom: 1;
    }
    DiskView .panel-summary {
        color: $text-muted;
        margin-bottom: 1;
    }
    DiskView #disk-controls {
        height: 3;
        margin-bottom: 1;
    }
    DiskView #disk-controls Button {
        margin-right: 1;
    }
    DiskView DataTable {
        height: auto;
        max-height: 10;
        margin-bottom: 1;
    }
    """

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)

    def compose(self) -> ComposeResult:
        with Vertical():
            with Horizontal(id="disk-controls"):
                yield Button("Refresh all", id="disk-refresh", variant="primary")

            yield Static("SMART health", classes="panel-title")
            yield Static("—", id="smart-summary", classes="panel-summary")
            smart_table = DataTable(id="smart-table", zebra_stripes=True, cursor_type="row")
            smart_table.add_columns("Device", "Kind", "Health", "Notable")
            yield smart_table

            yield Static("Ghost space (deleted but held open)", classes="panel-title")
            yield Static("—", id="ghost-summary", classes="panel-summary")
            ghost_table = DataTable(id="ghost-table", zebra_stripes=True, cursor_type="row")
            ghost_table.add_columns("PID", "Command", "User", "Size", "Path")
            yield ghost_table

            yield Static("Inode pressure", classes="panel-title")
            yield Static("—", id="inode-summary", classes="panel-summary")
            inode_table = DataTable(id="inode-table", zebra_stripes=True, cursor_type="row")
            inode_table.add_columns("Mount", "Filesystem", "Used %", "Used / Total")
            yield inode_table

    def on_mount(self) -> None:
        self._refresh_all()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        if event.button.id == "disk-refresh":
            self._refresh_all()

    def _refresh_all(self) -> None:
        self._refresh_smart()
        self._refresh_ghost()
        self._refresh_inodes()

    def _refresh_smart(self) -> None:
        reports = report_all()
        summary = self.query_one("#smart-summary", Static)
        table = self.query_one("#smart-table", DataTable)
        table.clear()
        if not reports:
            summary.update("no block devices found (or smartctl unavailable)")
            return
        failing = sum(1 for r in reports if not r.ok)
        summary.update(f"{len(reports)} device(s), {failing} not PASSED")
        for r in reports:
            notable_bits: list[str] = []
            if r.kind == "nvme":
                pct = r.attributes.get("percentage_used")
                if pct is not None:
                    notable_bits.append(f"used {pct}%")
                spare = r.attributes.get("available_spare")
                if spare is not None:
                    notable_bits.append(f"spare {spare}%")
            else:
                ra = r.attributes.get("reallocated_sectors")
                if ra is not None:
                    notable_bits.append(f"realloc {ra}")
                cps = r.attributes.get("current_pending_sector")
                if cps is not None and cps > 0:
                    notable_bits.append(f"pending {cps}")
                wear = r.attributes.get("ssd_wear_indicator")
                if wear is not None:
                    notable_bits.append(f"wear {wear}")
            if r.error:
                notable_bits.append(f"err: {r.error}")
            table.add_row(r.device, r.kind, r.health or "—", ", ".join(notable_bits) or "—")

    def _refresh_ghost(self) -> None:
        files = list_ghost_files()
        summary = self.query_one("#ghost-summary", Static)
        table = self.query_one("#ghost-table", DataTable)
        table.clear()
        if not files:
            summary.update("no deleted-but-held files (or lsof unavailable)")
            return
        summary.update(
            f"{len(files)} file(s) holding {_human_bytes(total_bytes(files))} reclaimable"
        )
        for f in sorted(files, key=lambda x: x.size_bytes, reverse=True):
            table.add_row(
                str(f.pid),
                f.command,
                f.user,
                _human_bytes(f.size_bytes),
                f.path,
            )

    def _refresh_inodes(self) -> None:
        rows = run_df_inodes()
        summary = self.query_one("#inode-summary", Static)
        table = self.query_one("#inode-table", DataTable)
        table.clear()
        if not rows:
            summary.update("df -i produced no parseable rows")
            return
        warn = [r for r in rows if r.is_warn]
        summary.update(f"{len(rows)} filesystem(s), {len(warn)} above 85% inode usage")
        for r in sorted(rows, key=lambda x: x.used_pct, reverse=True):
            table.add_row(
                r.mount,
                r.filesystem,
                f"{r.used_pct}%",
                f"{r.used:,} / {r.total:,}",
            )


def _human_bytes(n: int) -> str:
    for unit in ("B", "KB", "MB", "GB", "TB"):
        if n < 1024 or unit == "TB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{n} B"
        n //= 1024
    return f"{n} B"
