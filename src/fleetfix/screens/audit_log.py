"""Live tail of the local audit log.

Reads the local JSON-lines audit file written by `audit.logger.AuditLogger`
and renders the most recent records into a DataTable. The view refreshes
every 2 seconds so techs can watch their own actions land in real time.
"""

from __future__ import annotations

from pathlib import Path
from typing import Any

from textual.app import ComposeResult
from textual.widget import Widget
from textual.widgets import DataTable, Static

from fleetfix.audit.logger import read_recent

_REFRESH_INTERVAL_S = 2.0
_TAIL_LIMIT = 200


class AuditLogView(Widget):
    """DataTable-backed tail of the local audit log."""

    DEFAULT_CSS = """
    AuditLogView {
        height: 1fr;
    }
    AuditLogView #audit-empty {
        color: $text-muted;
        text-align: center;
        padding: 2 1;
    }
    AuditLogView DataTable {
        height: 1fr;
    }
    """

    def __init__(self, path: Path, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._path = path
        self._last_seq: int = -1

    def compose(self) -> ComposeResult:
        yield Static(
            f"No audit records yet. Writing to: {self._path}",
            id="audit-empty",
        )
        table = DataTable(id="audit-table", zebra_stripes=True, cursor_type="row")
        table.add_columns("Time", "Phase", "Action", "Operator", "Result")
        yield table

    def on_mount(self) -> None:
        self._refresh()
        self.set_interval(_REFRESH_INTERVAL_S, self._refresh)

    def _refresh(self) -> None:
        records = read_recent(self._path, limit=_TAIL_LIMIT)
        table = self.query_one("#audit-table", DataTable)
        empty = self.query_one("#audit-empty", Static)

        if not records:
            empty.display = True
            table.display = False
            return

        empty.display = False
        table.display = True

        new_records = [r for r in records if int(r.get("seq", 0)) > self._last_seq]
        if not new_records:
            return

        for record in new_records:
            table.add_row(*_format_row(record))
        self._last_seq = max(int(r.get("seq", 0)) for r in records)


def _format_row(record: dict[str, Any]) -> tuple[str, str, str, str, str]:
    ts = str(record.get("ts", "")).split("T", 1)
    time_part = ts[1].rstrip("Z") if len(ts) == 2 else str(record.get("ts", ""))
    phase = str(record.get("phase", "?"))
    action = str(record.get("action", "?"))
    op = record.get("operator") or {}
    op_label = str(op.get("unix_user") or "?")
    auth = op.get("auth_principal")
    if auth:
        op_label = f"{op_label} ({auth})"

    result = record.get("result")
    if result is None:
        result_label = "—"
    elif isinstance(result, dict):
        if result.get("ok") is True:
            result_label = "ok"
        elif result.get("ok") is False:
            err = result.get("error") or "error"
            result_label = f"FAIL: {err}"
        else:
            result_label = "—"
    else:
        result_label = str(result)
    return time_part, phase, action, op_label, result_label
