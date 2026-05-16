"""Reusable destructive-action confirmation modal.

Routes every destructive action through one shared screen with the same
shape: show what will happen, who's accountable, require typing a phrase,
and emit to the audit log before returning to the caller.
"""

from __future__ import annotations

from dataclasses import dataclass

from textual.app import ComposeResult
from textual.binding import Binding
from textual.containers import Container, Horizontal
from textual.screen import ModalScreen
from textual.widgets import Button, Input, Static

from fleetfix.audit.logger import Operator


@dataclass(frozen=True)
class ConfirmRequest:
    """The data a confirm modal needs to render and validate consent."""

    title: str
    description: str
    expected_phrase: str
    operator: Operator
    danger: str = "DELETE"  # cosmetic label for the danger pill


class ConfirmModal(ModalScreen[bool]):
    """Modal that returns True only after the operator types `expected_phrase`."""

    DEFAULT_CSS = """
    ConfirmModal {
        align: center middle;
    }
    ConfirmModal > Container {
        width: 70;
        height: auto;
        max-height: 80%;
        border: heavy $error;
        background: $surface;
        padding: 1 2;
    }
    ConfirmModal .confirm-title {
        text-style: bold;
        color: $error;
        margin-bottom: 1;
    }
    ConfirmModal .confirm-danger-pill {
        background: $error;
        color: $text;
        padding: 0 1;
        margin-bottom: 1;
    }
    ConfirmModal .confirm-description {
        margin-bottom: 1;
    }
    ConfirmModal .confirm-operator {
        color: $text-muted;
        margin-bottom: 1;
    }
    ConfirmModal .confirm-prompt {
        color: $accent;
        margin-top: 1;
    }
    ConfirmModal #confirm-error {
        color: $error;
        margin-top: 1;
        display: none;
    }
    ConfirmModal #confirm-buttons {
        height: 3;
        align: right middle;
        margin-top: 1;
    }
    ConfirmModal Button {
        margin-left: 1;
    }
    """

    BINDINGS = (Binding("escape", "cancel", "Cancel"),)

    def __init__(self, request: ConfirmRequest) -> None:
        super().__init__()
        self.request = request

    def compose(self) -> ComposeResult:
        with Container():
            yield Static(f"⚠  {self.request.title}", classes="confirm-title")
            yield Static(self.request.danger, classes="confirm-danger-pill")
            yield Static(self.request.description, classes="confirm-description")
            operator_line = (
                f"This action will be recorded as: "
                f"{self.request.operator.unix_user}"
                + (
                    f" ({self.request.operator.duo_principal})"
                    if self.request.operator.duo_principal
                    else ""
                )
                + (
                    f" from {self.request.operator.source_ip}"
                    if self.request.operator.source_ip
                    else ""
                )
            )
            yield Static(operator_line, classes="confirm-operator")
            yield Static(
                f"Type [b]{self.request.expected_phrase}[/b] to confirm:",
                classes="confirm-prompt",
            )
            yield Input(placeholder=self.request.expected_phrase, id="confirm-input")
            yield Static(
                "Phrase did not match. Try again or press Esc to cancel.", id="confirm-error"
            )
            with Horizontal(id="confirm-buttons"):
                yield Button("Cancel", id="confirm-cancel")
                yield Button("Confirm", variant="error", id="confirm-submit", disabled=True)

    def on_mount(self) -> None:
        self.query_one("#confirm-input", Input).focus()

    def on_input_changed(self, event: Input.Changed) -> None:
        if event.input.id != "confirm-input":
            return
        match = event.value.strip() == self.request.expected_phrase
        self.query_one("#confirm-submit", Button).disabled = not match
        # hide stale "did not match" error once user starts editing
        error = self.query_one("#confirm-error", Static)
        error.display = False

    def on_input_submitted(self, event: Input.Submitted) -> None:
        if event.input.id != "confirm-input":
            return
        self._attempt_confirm()

    def on_button_pressed(self, event: Button.Pressed) -> None:
        if event.button.id == "confirm-cancel":
            self.dismiss(False)
        elif event.button.id == "confirm-submit":
            self._attempt_confirm()

    def action_cancel(self) -> None:
        self.dismiss(False)

    def _attempt_confirm(self) -> None:
        value = self.query_one("#confirm-input", Input).value.strip()
        if value == self.request.expected_phrase:
            self.dismiss(True)
        else:
            self.query_one("#confirm-error", Static).display = True
