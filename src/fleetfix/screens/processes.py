"""Tier 2 Processes screen — top by RSS / CPU + signal sender.

Layout:

- Buttons row: "By RSS" / "By CPU" toggle ranking mode, "Refresh" reruns
  snapshot, "Kill selected" + "Force-kill selected" act on the focused row.
- Table: pid, user, command, RSS (MB), CPU%, cmdline (truncated).

Force-kill (SIGKILL) goes through a *second* confirm modal on top of the
shared ConfirmModal — the operator types "FORCE" instead of "KILL".
"""

from __future__ import annotations

import signal

from textual import work
from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widget import Widget
from textual.widgets import Button, DataTable, Static

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.modules.procs.killer import send_signal
from fleetfix.modules.procs.ranker import ProcInfo, snapshot, top_by_cpu, top_by_rss
from fleetfix.screens.confirm import ConfirmModal, ConfirmRequest

_TOP_N = 25


class ProcessesView(Widget):
    DEFAULT_CSS = """
    ProcessesView {
        layout: vertical;
        height: 1fr;
        padding: 1 1 0 1;
    }
    ProcessesView .panel-title {
        text-style: bold;
        color: $accent;
        margin-bottom: 1;
    }
    ProcessesView #procs-controls {
        height: 3;
        margin-bottom: 1;
    }
    ProcessesView #procs-controls Button {
        margin-right: 1;
    }
    ProcessesView #procs-result {
        height: auto;
        max-height: 4;
        margin-bottom: 1;
    }
    ProcessesView #procs-table {
        height: 1fr;
    }
    """

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._mode = "rss"
        self._procs: list[ProcInfo] = []

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Static("Top processes (Tier 2)", classes="panel-title")
            with Horizontal(id="procs-controls"):
                yield Button("By RSS", id="procs-by-rss", variant="primary")
                yield Button("By CPU", id="procs-by-cpu")
                yield Button("Refresh", id="procs-refresh")
                yield Button("SIGTERM", id="procs-term", variant="warning")
                yield Button("SIGKILL", id="procs-kill", variant="error")
            yield Static("Pick a row and choose an action.", id="procs-result")
            table = DataTable(id="procs-table", zebra_stripes=True, cursor_type="row")
            table.add_columns("PID", "User", "Comm", "RSS (MB)", "CPU%", "Command line")
            yield table

    def on_mount(self) -> None:
        self._refresh()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        bid = event.button.id
        if bid == "procs-by-rss":
            self._mode = "rss"
            self._render_table()
        elif bid == "procs-by-cpu":
            self._mode = "cpu"
            self._render_table()
        elif bid == "procs-refresh":
            self._refresh()
        elif bid == "procs-term":
            self._signal_selected(signal.SIGTERM, label="KILL")
        elif bid == "procs-kill":
            self._signal_selected(signal.SIGKILL, label="FORCE")

    def _refresh(self) -> None:
        # snapshot() samples CPU over a 0.2s window and walks /proc; running it
        # inline froze the TUI on each refresh. Spinner + thread worker keeps
        # the view responsive. The By RSS / By CPU toggle re-renders from the
        # cached snapshot, so it stays instant and needs no spinner.
        self.query_one("#procs-table", DataTable).loading = True
        self._load_procs()

    @work(thread=True, exclusive=True, group="procs-snapshot")
    def _load_procs(self) -> None:
        procs = snapshot(sample_interval_s=0.2)
        self.app.call_from_thread(self._apply_procs, procs)

    def _apply_procs(self, procs: list[ProcInfo]) -> None:
        self._procs = procs
        self.query_one("#procs-table", DataTable).loading = False
        self._render_table()

    def _render_table(self) -> None:
        ranked = (
            top_by_rss(self._procs, n=_TOP_N)
            if self._mode == "rss"
            else top_by_cpu(self._procs, n=_TOP_N)
        )
        table = self.query_one("#procs-table", DataTable)
        table.clear()
        for p in ranked:
            table.add_row(
                str(p.pid),
                p.user or "—",
                p.comm,
                f"{p.rss_bytes / 1024 / 1024:.1f}",
                f"{p.cpu_pct:.1f}",
                _truncate(p.cmdline, 80),
            )

    def _selected_pid(self) -> int | None:
        table = self.query_one("#procs-table", DataTable)
        if table.row_count == 0:
            return None
        row_idx = table.cursor_row
        if row_idx is None:
            return None
        try:
            cell = table.get_cell_at((row_idx, 0))  # type: ignore[arg-type]
        except Exception:
            return None
        try:
            return int(str(cell))
        except (TypeError, ValueError):
            return None

    def _signal_selected(self, sig: int, *, label: str) -> None:
        out = self.query_one("#procs-result", Static)
        pid = self._selected_pid()
        if pid is None:
            out.update("Select a row first.")
            return
        proc = next((p for p in self._procs if p.pid == pid), None)
        comm = proc.comm if proc else ""
        preview = f"kill -{signal.Signals(sig).name} {pid}  ({comm})"
        app = self.app
        if not hasattr(app, "audit"):
            out.update("Audit logger unavailable.")
            return
        audit: AuditLogger = app.audit  # type: ignore[attr-defined]

        def after_confirm(ok: bool | None) -> None:
            if not ok:
                out.update("Cancelled.")
                return
            result = send_signal(pid, sig=sig, audit=audit)
            if result.ok:
                out.update(f"[green]✓[/] Sent {signal.Signals(sig).name} to {pid}.")
            else:
                out.update(f"[red]✗[/] {result.error}")
            self._refresh()

        request = ConfirmRequest(
            title=f"Send {signal.Signals(sig).name}",
            description=preview,
            expected_phrase=label,
            operator=Operator.from_environment(),
            danger=signal.Signals(sig).name,
        )
        app.push_screen(ConfirmModal(request), after_confirm)


def _truncate(s: str, n: int) -> str:
    if len(s) <= n:
        return s
    return s[: n - 1] + "…"
