"""Tier 1 Storage screen — stale finder + env check + dotfile tree.

The full interactive tree-explorer lands in milestone 10; this view
already covers the two highest-value workflows in the spec:

  * "Find big old database dumps and log archives I can delete"
  * "Is my .env file present and well formed?"

The "Delete selected" button wires through screens/confirm.py and
modules/storage/safe_delete.py — both already exist from milestone 3.
"""

from __future__ import annotations

from pathlib import Path
from typing import TYPE_CHECKING

from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widget import Widget
from textual.widgets import Button, DataTable, Input, Static

from fleetfix.audit.logger import Operator
from fleetfix.config import InspectTarget
from fleetfix.modules.storage.env_check import check_env_file
from fleetfix.modules.storage.safe_delete import (
    BlacklistedPath,
    UnsafeDelete,
    safe_delete,
)
from fleetfix.modules.storage.stale import find_stale
from fleetfix.screens.confirm import ConfirmModal, ConfirmRequest

if TYPE_CHECKING:
    from fleetfix.app import FleetFixApp


def _human_bytes(n: int) -> str:
    """Compact size, e.g. 1.2G / 980M / 5.0K. Used for tightly-packed columns."""
    units = ("B", "K", "M", "G", "T")
    value = float(n)
    for unit in units:
        if value < 1024 or unit == units[-1]:
            return f"{value:.1f}{unit}" if unit != "B" else f"{int(value)}B"
        value /= 1024
    return f"{value:.1f}P"


class StorageView(Widget):
    """Storage screen — stale finder on top, env check below."""

    DEFAULT_CSS = """
    StorageView {
        layout: vertical;
        height: 1fr;
        padding: 1 1 0 1;
    }
    StorageView .panel-title {
        text-style: bold;
        color: $accent;
        margin-bottom: 1;
    }
    StorageView #stale-controls {
        height: 3;
        margin-bottom: 1;
    }
    StorageView #stale-controls Input {
        width: 40;
        margin-right: 1;
    }
    StorageView #stale-controls Button {
        margin-right: 1;
    }
    StorageView #stale-table {
        height: 1fr;
        margin-bottom: 1;
    }
    StorageView #env-block {
        height: auto;
        max-height: 14;
        border-top: solid $primary-darken-2;
        padding-top: 1;
    }
    StorageView #env-input {
        width: 60;
    }
    StorageView .status-ok { color: $success; }
    StorageView .status-bad { color: $error; }
    """

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._scan_root: Path = Path.home()
        self._candidates: list = []

    def compose(self) -> ComposeResult:
        yield Static("Stale artifacts under your home directory", classes="panel-title")
        with Horizontal(id="stale-controls"):
            yield Input(value=str(self._scan_root), id="stale-root", placeholder="Scan root")
            yield Input(value="30", id="stale-days", placeholder="Older than (days)")
            yield Button("Scan", id="stale-scan", variant="primary")
            yield Button("Delete selected", id="stale-delete", variant="error", disabled=True)
        table = DataTable(id="stale-table", zebra_stripes=True, cursor_type="row")
        table.add_columns("Size", "Age (days)", "Category", "Path")
        yield table

        with Vertical(id="env-block"):
            yield Static("Env / config file check", classes="panel-title")
            with Horizontal():
                yield Input(
                    value=str(self._scan_root / ".env"),
                    id="env-input",
                    placeholder="Path to .env",
                )
                yield Button("Check", id="env-check", variant="primary")
            yield Static("Enter a path and press Check.", id="env-result")

    def _default_root(self) -> Path:
        target = getattr(self.app, "inspect_target", None)
        if isinstance(target, InspectTarget):
            return target.home
        return Path.home()

    def on_mount(self) -> None:
        self._scan_root = self._default_root()
        self.query_one("#stale-root", Input).value = str(self._scan_root)
        self.query_one("#env-input", Input).value = str(self._scan_root / ".env")

    def on_button_pressed(self, event: Button.Pressed) -> None:
        bid = event.button.id
        if bid == "stale-scan":
            self._rescan()
        elif bid == "stale-delete":
            self._confirm_delete_selected()
        elif bid == "env-check":
            self._run_env_check()

    def _rescan(self) -> None:
        try:
            days = int(self.query_one("#stale-days", Input).value or "30")
        except ValueError:
            days = 30
        root_str = self.query_one("#stale-root", Input).value or str(self._default_root())
        self._scan_root = Path(root_str).expanduser()

        self._candidates = find_stale(self._scan_root, older_than_days=days)
        table = self.query_one("#stale-table", DataTable)
        table.clear()
        for c in self._candidates:
            table.add_row(
                _human_bytes(c.size_bytes),
                f"{c.age_days:.0f}",
                c.category,
                str(c.path),
            )
        self.query_one("#stale-delete", Button).disabled = not self._candidates

    def _run_env_check(self) -> None:
        path = Path(self.query_one("#env-input", Input).value or "").expanduser()
        result = check_env_file(path)
        out = self.query_one("#env-result", Static)
        if not result.exists:
            out.update(f"[bold red]✗[/] {path} does not exist")
            return
        if not result.readable:
            out.update(f"[bold red]✗[/] cannot read {path}: {result.issues}")
            return
        lines = [f"[bold]{path}[/] — {len(result.keys)} keys"]
        if result.missing_required:
            lines.append(f"[red]missing required:[/] {', '.join(result.missing_required)}")
        if result.issues:
            lines.append(f"[red]issues:[/] {len(result.issues)}")
            for issue in result.issues[:5]:
                lines.append(f"  line {issue.line_no}: {issue.message}")
        if result.ok:
            lines.append("[green]ok[/]")
        out.update("\n".join(lines))

    def _confirm_delete_selected(self) -> None:
        table = self.query_one("#stale-table", DataTable)
        row_index = table.cursor_row
        if row_index is None or row_index >= len(self._candidates):
            return
        candidate = self._candidates[row_index]
        app: FleetFixApp = self.app  # type: ignore[assignment]
        operator = Operator.from_environment()
        request = ConfirmRequest(
            title=f"Delete {candidate.path}",
            description=(
                f"Will permanently remove {_human_bytes(candidate.size_bytes)}. "
                f"This cannot be undone."
            ),
            expected_phrase="DELETE",
            operator=operator,
        )

        def after_confirm(approved: bool | None) -> None:
            if not approved:
                return
            try:
                safe_delete(candidate.path, app.audit)
            except (BlacklistedPath, UnsafeDelete, OSError) as exc:
                self.notify(f"Delete refused: {exc}", severity="error")
                return
            self.notify(f"Deleted {candidate.path}", severity="information")
            self._rescan()

        app.push_screen(ConfirmModal(request), after_confirm)
