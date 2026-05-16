"""Textual pilot tests for the AuditLogView."""

from __future__ import annotations

import json
from pathlib import Path

import pytest
from textual.app import App, ComposeResult
from textual.widgets import DataTable, Static

from fleetfix.screens.audit_log import AuditLogView


def _write_records(path: Path, records: list[dict[str, object]]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as fh:
        for record in records:
            fh.write(json.dumps(record) + "\n")


class _Harness(App[None]):
    def __init__(self, path: Path) -> None:
        super().__init__()
        self._path = path

    def compose(self) -> ComposeResult:
        yield AuditLogView(self._path)


@pytest.mark.asyncio
async def test_view_shows_empty_message_when_no_records(tmp_path: Path) -> None:
    audit_path = tmp_path / "audit.log"
    app = _Harness(audit_path)
    async with app.run_test() as pilot:
        await pilot.pause()
        empty = app.query_one("#audit-empty", Static)
        table = app.query_one("#audit-table", DataTable)
        assert empty.display is True
        assert table.display is False


@pytest.mark.asyncio
async def test_view_renders_existing_records(tmp_path: Path) -> None:
    audit_path = tmp_path / "audit.log"
    _write_records(
        audit_path,
        [
            {
                "ts": "2026-05-16T10:00:00.000Z",
                "host": "h",
                "session_id": "s",
                "call_id": "c1",
                "seq": 1,
                "phase": "event",
                "operator": {"unix_user": "appuser", "duo_principal": None, "source_ip": None},
                "action": "fleetfix.launch",
                "target": {},
                "result": None,
                "fleetfix_version": "0.1.0",
            },
            {
                "ts": "2026-05-16T10:00:01.000Z",
                "host": "h",
                "session_id": "s",
                "call_id": "c2",
                "seq": 2,
                "phase": "intent",
                "operator": {"unix_user": "appuser", "duo_principal": None, "source_ip": None},
                "action": "storage.delete_file",
                "target": {"path": "/home/appuser/old.sql"},
                "result": None,
                "fleetfix_version": "0.1.0",
            },
        ],
    )
    app = _Harness(audit_path)
    async with app.run_test() as pilot:
        await pilot.pause()
        empty = app.query_one("#audit-empty", Static)
        table = app.query_one("#audit-table", DataTable)
        assert empty.display is False
        assert table.display is True
        assert table.row_count == 2


@pytest.mark.asyncio
async def test_view_picks_up_records_added_after_mount(tmp_path: Path) -> None:
    audit_path = tmp_path / "audit.log"
    app = _Harness(audit_path)
    async with app.run_test() as pilot:
        await pilot.pause()
        view = app.query_one(AuditLogView)
        _write_records(
            audit_path,
            [
                {
                    "ts": "2026-05-16T10:00:00.000Z",
                    "host": "h",
                    "session_id": "s",
                    "call_id": "c1",
                    "seq": 1,
                    "phase": "event",
                    "operator": {"unix_user": "appuser", "duo_principal": None, "source_ip": None},
                    "action": "fleetfix.launch",
                    "target": {},
                    "result": None,
                    "fleetfix_version": "0.1.0",
                }
            ],
        )
        view._refresh()
        await pilot.pause()
        table = app.query_one("#audit-table", DataTable)
        assert table.row_count == 1
