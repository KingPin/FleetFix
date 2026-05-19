"""Top bar widget — shows host identity, FleetFix version, privilege state, update banner."""

from __future__ import annotations

from textual.app import ComposeResult
from textual.containers import Horizontal
from textual.reactive import reactive
from textual.widget import Widget
from textual.widgets import Static

from fleetfix import __version__
from fleetfix.config import HostInfo
from fleetfix.privilege import PrivilegeState


class TopBar(Widget):
    DEFAULT_CSS = """
    TopBar {
        height: 3;
        background: $panel;
        color: $text;
        padding: 0 1;
        border-bottom: solid $primary-darken-1;
    }
    TopBar > Horizontal {
        height: 1fr;
        align: left middle;
    }
    TopBar .topbar-segment {
        padding: 0 1;
    }
    TopBar .topbar-version {
        color: $accent;
    }
    TopBar .topbar-host {
        color: $success;
    }
    TopBar .topbar-mode-readonly {
        color: $warning;
    }
    TopBar .topbar-privilege-locked {
        color: $error;
    }
    TopBar .topbar-update {
        color: $warning;
    }
    TopBar .topbar-target {
        color: $accent;
    }
    """

    update_available: reactive[str | None] = reactive(None)

    def __init__(
        self,
        *,
        host: HostInfo,
        privilege: PrivilegeState,
        read_only: bool,
        inspect_target_user: str | None = None,
    ) -> None:
        super().__init__()
        self.host = host
        self.privilege = privilege
        self.read_only = read_only
        self.inspect_target_user = inspect_target_user

    def compose(self) -> ComposeResult:
        version_label = Static(f"FleetFix {__version__}", classes="topbar-segment topbar-version")
        host_label = Static(
            f"{self.host.hostname}  {self.host.os_pretty}  ({self.host.kernel} {self.host.arch})",
            classes="topbar-segment topbar-host",
        )

        privilege_class = "topbar-segment"
        if self.privilege.is_root:
            privilege_text = "root"
        elif self.privilege.can_tier2:
            privilege_text = "sudo-ready"
        else:
            privilege_text = "Tier 1 only (no sudo)"
            privilege_class += " topbar-privilege-locked"
        privilege_label = Static(privilege_text, classes=privilege_class)

        widgets = [version_label, host_label, privilege_label]
        if self.inspect_target_user is not None:
            widgets.append(
                Static(
                    f"Inspecting: {self.inspect_target_user}",
                    classes="topbar-segment topbar-target",
                )
            )
        if self.read_only:
            widgets.append(Static("READ-ONLY", classes="topbar-segment topbar-mode-readonly"))

        self._update_label = Static("", classes="topbar-segment topbar-update", id="update-banner")
        self._update_label.display = False
        widgets.append(self._update_label)

        yield Horizontal(*widgets)

    def watch_update_available(self, new_value: str | None) -> None:
        label = self.query_one("#update-banner", Static)
        if new_value:
            label.update(f"Update available: {new_value}  (press U to update)")
            label.display = True
        else:
            label.update("")
            label.display = False
