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
from fleetfix.audit.otel import load_otel_config, make_sink
from fleetfix.config import HostInfo, detect_host, resolve_audit_path
from fleetfix.privilege import PrivilegeState
from fleetfix.privilege import detect as detect_privilege
from fleetfix.screens.audit_log import AuditLogView
from fleetfix.screens.confirm import ConfirmModal, ConfirmRequest
from fleetfix.screens.dashboard import DashboardView
from fleetfix.screens.disk import DiskView
from fleetfix.screens.docker import DockerView
from fleetfix.screens.network import NetworkView
from fleetfix.screens.placeholder import PlaceholderView
from fleetfix.screens.processes import ProcessesView
from fleetfix.screens.services import ServicesView
from fleetfix.screens.storage import StorageView
from fleetfix.updater.checker import ReleaseInfo, check_for_update
from fleetfix.updater.installer import apply_update
from fleetfix.widgets.nav import NAV_ITEMS, Nav
from fleetfix.widgets.topbar import TopBar


@dataclass(frozen=True)
class AppContext:
    version: str
    read_only: bool


_VIEW_MILESTONES: dict[str, str] = {}


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

    def __init__(self, *, read_only: bool = False, check_for_update_on_mount: bool = True) -> None:
        super().__init__()
        self.ctx = AppContext(version=__version__, read_only=read_only)
        self._check_for_update_on_mount = check_for_update_on_mount
        self.host: HostInfo = detect_host()
        self.privilege: PrivilegeState = detect_privilege()
        self.audit_path = resolve_audit_path()
        self._otel_sink = make_sink(load_otel_config())
        self.audit = AuditLogger(
            self.audit_path,
            operator=Operator.from_environment(),
            sink=self._otel_sink.emit if self._otel_sink is not None else None,
        )
        self.update_release: ReleaseInfo | None = None

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
                    if item.key == "storage":
                        yield StorageView(id="view-storage")
                        continue
                    if item.key == "network":
                        yield NetworkView(id="view-network")
                        continue
                    if item.key == "disk":
                        yield DiskView(id="view-disk")
                        continue
                    if item.key == "docker":
                        yield DockerView(id="view-docker")
                        continue
                    if item.key == "processes":
                        yield ProcessesView(id="view-processes")
                        continue
                    if item.key == "services":
                        yield ServicesView(id="view-services")
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
        if self._check_for_update_on_mount:
            self.run_worker(
                self._check_for_update, thread=True, exclusive=True, name="update-check"
            )

    def _check_for_update(self) -> None:
        """Background fire-and-forget — silent on failure (see checker docs)."""
        release = check_for_update(self.ctx.version)
        if release is None:
            return
        self.call_from_thread(self._on_update_found, release)

    def _on_update_found(self, release: ReleaseInfo) -> None:
        self.update_release = release
        topbar = self.query_one(TopBar)
        topbar.update_available = release.tag

    def on_unmount(self) -> None:
        self.audit.event("fleetfix.exit", host=self.host.hostname)
        if self._otel_sink is not None:
            self._otel_sink.shutdown()

    def on_nav_selected(self, message: Nav.Selected) -> None:
        self.action_switch(message.key)

    def action_switch(self, key: str) -> None:
        switcher = self.query_one("#content", ContentSwitcher)
        target = f"view-{key}"
        switcher.current = target

    def action_show_update(self) -> None:
        release = self.update_release
        if release is None:
            self.notify("No update available.", severity="information")
            return
        request = ConfirmRequest(
            title=f"Update FleetFix to {release.tag}",
            description=(
                f"Replace {self.ctx.version} → {release.version}.\n\n"
                f"Source: {release.asset_url}\n"
                "The binary is sha256-verified before swap. Sudo is required to "
                "write /usr/local/bin/fleetfix; you must restart FleetFix to use "
                "the new version."
            ),
            expected_phrase="UPDATE",
            operator=self.audit.operator,
            danger="UPDATE",
        )

        def after_confirm(ok: bool | None) -> None:
            if not ok:
                return
            self.notify(f"Updating to {release.tag}…", severity="information")
            self.run_worker(
                lambda: self._apply_update(release),
                thread=True,
                exclusive=True,
                name="update-apply",
            )

        self.push_screen(ConfirmModal(request), after_confirm)

    def _apply_update(self, release: ReleaseInfo) -> None:
        result = apply_update(release, audit=self.audit)
        if result.ok:
            self.call_from_thread(
                self.notify,
                f"Update to {release.tag} applied. Restart FleetFix to use the new version.",
                severity="information",
            )
        else:
            self.call_from_thread(
                self.notify,
                f"Update failed: {result.error}",
                severity="error",
            )
