"""Tier 2 Docker screen — container dashboard + log truncate + prune.

Two panels stacked vertically:

1. Container table (pid -> name, image, state, status, restart count). The
   "restart loop" column highlights any container that crossed the
   threshold inside the recent window.
2. System-df summary line + "Prune images" / "Prune volumes" buttons.

The "Truncate log" button operates on the currently-focused container row.
Every destructive action funnels through `ConfirmModal` and is audit-logged
by the underlying module.
"""

from __future__ import annotations

from textual import work
from textual.app import ComposeResult
from textual.containers import Horizontal, Vertical
from textual.widget import Widget
from textual.widgets import Button, DataTable, Static

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.modules.docker.dashboard import Container, list_containers
from fleetfix.modules.docker.hygiene import DfRow, prune_images, prune_volumes, system_df
from fleetfix.modules.docker.truncate import truncate_log
from fleetfix.screens.confirm import ConfirmModal, ConfirmRequest


class DockerView(Widget):
    DEFAULT_CSS = """
    DockerView {
        layout: vertical;
        height: 1fr;
        padding: 1 1 0 1;
    }
    DockerView .panel-title {
        text-style: bold;
        color: $accent;
        margin-top: 1;
        margin-bottom: 1;
    }
    DockerView .panel-summary {
        color: $text-muted;
        margin-bottom: 1;
    }
    DockerView #docker-controls {
        height: 3;
        margin-bottom: 1;
    }
    DockerView #docker-controls Button {
        margin-right: 1;
    }
    DockerView #docker-table {
        height: auto;
        max-height: 14;
        margin-bottom: 1;
    }
    DockerView #docker-result {
        margin-bottom: 1;
    }
    """

    def __init__(self, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._containers: list[Container] = []

    def compose(self) -> ComposeResult:
        with Vertical():
            yield Static("Containers (Tier 2)", classes="panel-title")
            yield Static("—", id="docker-summary", classes="panel-summary")
            with Horizontal(id="docker-controls"):
                yield Button("Refresh", id="docker-refresh", variant="primary")
                yield Button("Truncate log", id="docker-truncate", variant="warning")
                yield Button("Prune images", id="docker-prune-images", variant="warning")
                yield Button("Prune volumes", id="docker-prune-volumes", variant="warning")
            table = DataTable(id="docker-table", zebra_stripes=True, cursor_type="row")
            table.add_columns("Container", "Image", "State", "Status", "Restarts", "Flag")
            yield table
            yield Static("Pick a row and choose an action.", id="docker-result")

            yield Static("System disk usage", classes="panel-title")
            yield Static("—", id="docker-df-summary", classes="panel-summary")
            df_table = DataTable(id="docker-df-table", zebra_stripes=True, cursor_type="row")
            df_table.add_columns("Type", "Active / Total", "Size", "Reclaimable")
            yield df_table

    def on_mount(self) -> None:
        self._refresh_all()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        bid = event.button.id
        if bid == "docker-refresh":
            self._refresh_all()
        elif bid == "docker-truncate":
            self._truncate_selected()
        elif bid == "docker-prune-images":
            self._prune_images()
        elif bid == "docker-prune-volumes":
            self._prune_volumes()

    def _refresh_all(self) -> None:
        self._refresh_containers()
        self._refresh_df()

    # `docker ps` and `docker system df` shell out and can take a beat on a
    # busy host. Each panel shows a spinner and runs its query in a thread
    # worker so opening the Docker view never freezes the TUI.

    def _refresh_containers(self) -> None:
        self.query_one("#docker-table", DataTable).loading = True
        self._load_containers()

    @work(thread=True, exclusive=True, group="docker-containers")
    def _load_containers(self) -> None:
        containers = list_containers()
        self.app.call_from_thread(self._apply_containers, containers)

    def _apply_containers(self, containers: list[Container]) -> None:
        self._containers = containers
        summary = self.query_one("#docker-summary", Static)
        table = self.query_one("#docker-table", DataTable)
        table.loading = False
        table.clear()
        if not self._containers:
            summary.update("no containers found (or docker unavailable)")
            return
        running = sum(1 for c in self._containers if c.state == "running")
        loops = sum(1 for c in self._containers if c.is_restart_loop)
        summary.update(
            f"{len(self._containers)} container(s), {running} running, {loops} in restart loop"
        )
        for c in self._containers:
            flag = "⚠ loop" if c.is_restart_loop else ""
            table.add_row(
                c.name or c.id[:12],
                c.image,
                c.state,
                c.status,
                str(c.restart_count),
                flag,
            )

    def _refresh_df(self) -> None:
        self.query_one("#docker-df-table", DataTable).loading = True
        self._load_df()

    @work(thread=True, exclusive=True, group="docker-df")
    def _load_df(self) -> None:
        rows = system_df()
        self.app.call_from_thread(self._apply_df, rows)

    def _apply_df(self, rows: list[DfRow]) -> None:
        summary = self.query_one("#docker-df-summary", Static)
        table = self.query_one("#docker-df-table", DataTable)
        table.loading = False
        table.clear()
        if not rows:
            summary.update("docker system df produced no rows")
            return
        total_reclaimable = sum(r.reclaimable_bytes for r in rows)
        summary.update(f"{_human_bytes(total_reclaimable)} reclaimable across all categories")
        for r in rows:
            table.add_row(
                r.type,
                f"{r.active} / {r.total_count}",
                _human_bytes(r.size_bytes),
                _format_reclaimable(r),
            )

    def _selected_container(self) -> Container | None:
        table = self.query_one("#docker-table", DataTable)
        if table.row_count == 0 or table.cursor_row is None:
            return None
        if table.cursor_row >= len(self._containers):
            return None
        return self._containers[table.cursor_row]

    def _truncate_selected(self) -> None:
        out = self.query_one("#docker-result", Static)
        container = self._selected_container()
        if container is None:
            out.update("Select a container first.")
            return
        audit = self._audit_or_none()
        if audit is None:
            out.update("Audit logger unavailable.")
            return
        request = ConfirmRequest(
            title="Truncate container log",
            description=f"truncate -s 0 {container.log_path or '<unknown path>'}",
            expected_phrase="TRUNCATE",
            operator=Operator.from_environment(),
            danger="TRUNCATE",
        )

        def after(ok: bool | None) -> None:
            if not ok:
                out.update("Cancelled.")
                return
            result = truncate_log(container.id, audit=audit, container_name=container.name)
            if result.ok:
                out.update(
                    f"[green]✓[/] Truncated {container.name}: "
                    f"{_human_bytes(result.bytes_freed)} freed."
                )
            else:
                out.update(f"[red]✗[/] {result.error}")
            self._refresh_all()

        self.app.push_screen(ConfirmModal(request), after)

    def _prune_images(self) -> None:
        self._run_prune("images", "PRUNE", prune_images)

    def _prune_volumes(self) -> None:
        self._run_prune("volumes", "PRUNE", prune_volumes)

    def _run_prune(self, target: str, phrase: str, prune_fn) -> None:  # type: ignore[no-untyped-def]
        out = self.query_one("#docker-result", Static)
        audit = self._audit_or_none()
        if audit is None:
            out.update("Audit logger unavailable.")
            return
        request = ConfirmRequest(
            title=f"Prune {target}",
            description=f"docker {target[:-1] if target.endswith('s') else target} prune -f",
            expected_phrase=phrase,
            operator=Operator.from_environment(),
            danger="PRUNE",
        )

        def after(ok: bool | None) -> None:
            if not ok:
                out.update("Cancelled.")
                return
            result = prune_fn(audit=audit)
            if result.ok:
                out.update(
                    f"[green]✓[/] Pruned {target}: "
                    f"{_human_bytes(result.bytes_reclaimed)} reclaimed."
                )
            else:
                out.update(f"[red]✗[/] {result.error}")
            self._refresh_df()

        self.app.push_screen(ConfirmModal(request), after)

    def _audit_or_none(self) -> AuditLogger | None:
        app = self.app
        if not hasattr(app, "audit"):
            return None
        return app.audit  # type: ignore[attr-defined,no-any-return]


def _format_reclaimable(row: DfRow) -> str:
    if row.reclaimable_pct:
        return f"{_human_bytes(row.reclaimable_bytes)} ({row.reclaimable_pct}%)"
    return _human_bytes(row.reclaimable_bytes)


def _human_bytes(n: int) -> str:
    units = ("B", "KB", "MB", "GB", "TB")
    size = float(n)
    for unit in units:
        if size < 1024 or unit == units[-1]:
            return f"{size:.1f} {unit}" if unit != "B" else f"{int(size)} B"
        size /= 1024
    return f"{int(size)} B"
