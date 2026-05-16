"""Textual App root for FleetFix."""

from __future__ import annotations

from dataclasses import dataclass
from typing import ClassVar

from textual.app import App, ComposeResult
from textual.binding import Binding, BindingType
from textual.containers import Horizontal
from textual.widgets import ContentSwitcher, Footer

from fleetfix import __version__
from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.config import HostInfo, detect_host, resolve_audit_path
from fleetfix.privilege import PrivilegeState
from fleetfix.privilege import detect as detect_privilege
from fleetfix.screens.audit_log import AuditLogView
from fleetfix.screens.dashboard import DashboardView
from fleetfix.screens.placeholder import PlaceholderView
from fleetfix.widgets.nav import NAV_ITEMS, Nav
from fleetfix.widgets.topbar import TopBar


@dataclass(frozen=True)
class AppContext:
    version: str
    read_only: bool


_VIEW_MILESTONES = {
    "storage": "4",
    "network": "4",
    "docker": "6",
    "processes": "5",
    "services": "7",
}


class FleetFixApp(App[None]):
    TITLE = "FleetFix"
    CSS = """
    Horizontal#main {
        height: 1fr;
    }
    ContentSwitcher#content {
        height: 1fr;
        width: 1fr;
    }
    """
    BINDINGS: ClassVar[list[BindingType]] = [
        Binding("q", "quit", "Quit"),
        Binding("d", "switch('dashboard')", "Dashboard"),
        Binding("u", "show_update", "Update"),
    ]

    def __init__(self, *, read_only: bool = False) -> None:
        super().__init__()
        self.ctx = AppContext(version=__version__, read_only=read_only)
        self.host: HostInfo = detect_host()
        self.privilege: PrivilegeState = detect_privilege()
        self.audit_path = resolve_audit_path()
        self.audit = AuditLogger(self.audit_path, operator=Operator.from_environment())

    def compose(self) -> ComposeResult:
        yield TopBar(host=self.host, privilege=self.privilege, read_only=self.ctx.read_only)
        with Horizontal(id="main"):
            yield Nav(can_tier2=self.privilege.can_tier2)
            with ContentSwitcher(initial="view-dashboard", id="content"):
                yield DashboardView(id="view-dashboard")
                for item in NAV_ITEMS:
                    if item.key == "dashboard":
                        continue
                    if item.key == "audit":
                        yield AuditLogView(self.audit_path, id="view-audit")
                        continue
                    yield PlaceholderView(
                        item.label,
                        _VIEW_MILESTONES.get(item.key, "TBD"),
                        id=f"view-{item.key}",
                    )
        yield Footer()

    def on_mount(self) -> None:
        self.audit.event(
            "fleetfix.launch",
            host=self.host.hostname,
            os=self.host.os_pretty,
            kernel=self.host.kernel,
            version=self.ctx.version,
            read_only=self.ctx.read_only,
            can_tier2=self.privilege.can_tier2,
        )

    def on_unmount(self) -> None:
        self.audit.event("fleetfix.exit", host=self.host.hostname)

    def on_nav_selected(self, message: Nav.Selected) -> None:
        self.action_switch(message.key)

    def action_switch(self, key: str) -> None:
        switcher = self.query_one("#content", ContentSwitcher)
        target = f"view-{key}"
        switcher.current = target

    def action_show_update(self) -> None:
        self.notify("Updater wires up in milestone 9", severity="information")
