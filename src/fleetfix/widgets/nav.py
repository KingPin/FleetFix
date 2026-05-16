"""Left navigation sidebar."""

from __future__ import annotations

from dataclasses import dataclass

from textual.app import ComposeResult
from textual.containers import Vertical
from textual.message import Message
from textual.widget import Widget
from textual.widgets import Button


@dataclass(frozen=True)
class NavItem:
    key: str
    label: str
    tier2: bool = False


NAV_ITEMS: tuple[NavItem, ...] = (
    NavItem("dashboard", "Dashboard"),
    NavItem("storage", "Storage"),
    NavItem("network", "Network"),
    NavItem("docker", "Docker", tier2=True),
    NavItem("processes", "Processes", tier2=True),
    NavItem("services", "Services", tier2=True),
    NavItem("audit", "Audit Log"),
)


class Nav(Widget):
    DEFAULT_CSS = """
    Nav {
        width: 22;
        background: $boost;
        padding: 1;
        border-right: solid $primary-darken-1;
    }
    Nav Button {
        width: 100%;
        margin-bottom: 1;
    }
    Nav .nav-locked {
        opacity: 0.5;
    }
    """

    class Selected(Message):
        def __init__(self, key: str) -> None:
            super().__init__()
            self.key = key

    def __init__(self, *, can_tier2: bool) -> None:
        super().__init__()
        self.can_tier2 = can_tier2

    def compose(self) -> ComposeResult:
        with Vertical():
            for item in NAV_ITEMS:
                locked = item.tier2 and not self.can_tier2
                label = f"{item.label}  🔒" if locked else item.label
                button = Button(label, id=f"nav-{item.key}", disabled=locked)
                if locked:
                    button.add_class("nav-locked")
                    button.tooltip = "Sudo required"
                yield button

    def on_button_pressed(self, event: Button.Pressed) -> None:
        if event.button.id and event.button.id.startswith("nav-"):
            key = event.button.id.removeprefix("nav-")
            self.post_message(self.Selected(key))
