"""Stand-in view for nav targets that land in later milestones."""

from __future__ import annotations

from textual.app import ComposeResult
from textual.containers import Center, Middle
from textual.widget import Widget
from textual.widgets import Static


class PlaceholderView(Widget):
    DEFAULT_CSS = """
    PlaceholderView {
        height: 1fr;
    }
    PlaceholderView Static {
        color: $text-muted;
        text-align: center;
    }
    """

    def __init__(self, label: str, milestone: str, *, id: str | None = None) -> None:
        super().__init__(id=id)
        self._label = label
        self._milestone = milestone

    def compose(self) -> ComposeResult:
        with Middle():
            with Center():
                yield Static(
                    f"[b]{self._label}[/b]\n\nLands in milestone {self._milestone}.",
                )
