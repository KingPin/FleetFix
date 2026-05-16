"""Tier 2 Services screen — failed units + journal tail + boot blame.

Two stacked panels:

1. Failed units table. Clicking a row populates the journal-tail
   `Static` below it with the last 100 lines from that unit's journal.
2. Boot blame table, ranked by duration descending, with units past the
   5-second outlier threshold flagged.
"""

from __future__ import annotations

from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widget import Widget
from textual.widgets import Button, DataTable, Static

from fleetfix.modules.services.boot import BlameEntry, blame
from fleetfix.modules.services.failed import FailedUnit, list_failed_units
from fleetfix.modules.services.journal import journal_tail


class ServicesView(Widget):
    DEFAULT_CSS = """
    ServicesView {
        layout: vertical;
        height: 1fr;
        padding: 1 1 0 1;
    }
    ServicesView .panel-title {
        text-style: bold;
        color: $accent;
        margin-top: 1;
        margin-bottom: 1;
    }
    ServicesView .panel-summary {
        color: $text-muted;
        margin-bottom: 1;
    }
    ServicesView #services-controls {
        height: 3;
        margin-bottom: 1;
    }
    ServicesView #services-controls Button {
        margin-right: 1;
    }
    ServicesView #failed-table {
        height: auto;
        max-height: 8;
        margin-bottom: 1;
    }
    ServicesView #blame-table {
        height: auto;
        max-height: 10;
        margin-bottom: 1;
    }
    ServicesView #journal-output {
        height: auto;
        max-height: 12;
        background: $panel;
        color: $text;
        padding: 1;
        margin-bottom: 1;
    }
    """

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._failed: list[FailedUnit] = []
        self._blame: list[BlameEntry] = []

    def compose(self) -> ComposeResult:
        with Vertical():
            with Horizontal(id="services-controls"):
                yield Button("Refresh", id="services-refresh", variant="primary")
                yield Button("Tail journal", id="services-journal", variant="default")

            yield Static("Failed units", classes="panel-title")
            yield Static("—", id="failed-summary", classes="panel-summary")
            failed_table = DataTable(id="failed-table", zebra_stripes=True, cursor_type="row")
            failed_table.add_columns("Unit", "Sub", "Description")
            yield failed_table

            yield Static("(Tail journal will show output here.)", id="journal-output")

            yield Static("Boot blame", classes="panel-title")
            yield Static("—", id="blame-summary", classes="panel-summary")
            blame_table = DataTable(id="blame-table", zebra_stripes=True, cursor_type="row")
            blame_table.add_columns("Unit", "Duration", "Outlier")
            yield blame_table

    def on_mount(self) -> None:
        self._refresh_all()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        if event.button.id == "services-refresh":
            self._refresh_all()
        elif event.button.id == "services-journal":
            self._tail_selected_journal()

    def _refresh_all(self) -> None:
        self._refresh_failed()
        self._refresh_blame()

    def _refresh_failed(self) -> None:
        self._failed = list_failed_units()
        summary = self.query_one("#failed-summary", Static)
        table = self.query_one("#failed-table", DataTable)
        table.clear()
        if not self._failed:
            summary.update("no failed units")
            return
        summary.update(f"{len(self._failed)} failed unit(s)")
        for u in self._failed:
            table.add_row(u.name, u.sub, u.description)

    def _refresh_blame(self) -> None:
        self._blame = blame()
        summary = self.query_one("#blame-summary", Static)
        table = self.query_one("#blame-table", DataTable)
        table.clear()
        if not self._blame:
            summary.update("systemd-analyze blame produced no rows")
            return
        outliers = sum(1 for e in self._blame if e.is_outlier)
        summary.update(f"{len(self._blame)} unit(s), {outliers} above 5s")
        # Show worst first.
        for e in sorted(self._blame, key=lambda x: x.duration_ms, reverse=True):
            flag = "⚠ slow" if e.is_outlier else ""
            table.add_row(e.unit, _human_ms(e.duration_ms), flag)

    def _tail_selected_journal(self) -> None:
        out = self.query_one("#journal-output", Static)
        table = self.query_one("#failed-table", DataTable)
        if table.row_count == 0 or table.cursor_row is None:
            out.update("(select a failed unit row first.)")
            return
        if table.cursor_row >= len(self._failed):
            out.update("(no unit at that row.)")
            return
        unit = self._failed[table.cursor_row].name
        text = journal_tail(unit)
        out.update(text or f"(no recent journal output for {unit})")


def _human_ms(ms: int) -> str:
    if ms < 1000:
        return f"{ms} ms"
    seconds = ms / 1000
    if seconds < 60:
        return f"{seconds:.2f} s"
    minutes = int(seconds // 60)
    rem = seconds - (minutes * 60)
    return f"{minutes}m {rem:.2f}s"
