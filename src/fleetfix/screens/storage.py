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

from textual import work
from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widget import Widget
from textual.widgets import Button, DataTable, Input, Static

from fleetfix.audit.logger import Operator
from fleetfix.modules.storage.env_check import check_env_file
from fleetfix.modules.storage.safe_delete import (
    BlacklistedPath,
    UnsafeDelete,
    safe_delete,
)
from fleetfix.modules.storage.stale import StaleCandidate, find_stale
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
        self._candidates: list[StaleCandidate] = []

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
        app: FleetFixApp = self.app  # type: ignore[assignment]
        if app.inspect_target is not None:
            return app.inspect_target.home
        return Path.home()

    def on_mount(self) -> None:
        self._scan_root = self._default_root()
        self.query_one("#stale-root", Input).value = str(self._scan_root)
        self.query_one("#env-input", Input).value = str(self._scan_root / ".env")

    def on_button_pressed(self, event: Button.Pressed) -> None:
        bid = event.button.id
        if bid == "stale-scan":
            self._start_scan()
        elif bid == "stale-delete":
            self._confirm_delete_selected()
        elif bid == "env-check":
            self._run_env_check()

    def _start_scan(self) -> None:
        """Read scan params on the UI thread, then hand the walk to a worker.

        ``find_stale`` recurses an entire directory tree; running it inline in
        the button handler blocked Textual's event loop and froze the whole
        TUI on real homes. We read the inputs here (DOM access must stay on the
        event loop), disable the controls, and offload the walk to a thread.
        """
        try:
            days = int(self.query_one("#stale-days", Input).value or "30")
        except ValueError:
            days = 30
        root_str = self.query_one("#stale-root", Input).value or str(self._default_root())
        self._scan_root = Path(root_str).expanduser()

        scan_btn = self.query_one("#stale-scan", Button)
        scan_btn.disabled = True
        scan_btn.label = "Scanning…"
        self.query_one("#stale-delete", Button).disabled = True
        self.query_one("#stale-table", DataTable).loading = True
        self._scan_worker(self._scan_root, days)

    @work(thread=True, exclusive=True, group="stale-scan")
    def _scan_worker(self, root: Path, days: int) -> None:
        """Run the (blocking) filesystem walk off the event loop.

        On an unexpected error we still route back to the UI thread so the
        Scan button never strands on "Scanning…" — find_stale already
        swallows per-file OSErrors, so reaching the except is rare.
        """
        try:
            candidates = find_stale(root, older_than_days=days)
        except OSError as exc:
            self.app.call_from_thread(self._scan_failed, str(exc))
            return
        self.app.call_from_thread(self._apply_scan_results, candidates)

    def _scan_failed(self, message: str) -> None:
        """Restore the controls and surface the error — back on the UI thread."""
        scan_btn = self.query_one("#stale-scan", Button)
        scan_btn.disabled = False
        scan_btn.label = "Scan"
        self.query_one("#stale-table", DataTable).loading = False
        self.notify(f"Scan failed: {message}", severity="error")

    def _apply_scan_results(self, candidates: list[StaleCandidate]) -> None:
        """Populate the table and re-enable controls — back on the UI thread."""
        self._candidates = candidates
        table = self.query_one("#stale-table", DataTable)
        table.loading = False
        table.clear()
        for c in candidates:
            table.add_row(
                _human_bytes(c.size_bytes),
                f"{c.age_days:.0f}",
                c.category,
                str(c.path),
            )
        scan_btn = self.query_one("#stale-scan", Button)
        scan_btn.disabled = False
        scan_btn.label = "Scan"
        self.query_one("#stale-delete", Button).disabled = not candidates

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
            self._start_scan()

        app.push_screen(ConfirmModal(request), after_confirm)
