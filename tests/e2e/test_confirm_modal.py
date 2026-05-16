"""Textual pilot tests for the destructive-action confirm modal."""

from __future__ import annotations

import pytest
from textual.app import App, ComposeResult
from textual.widgets import Static

from fleetfix.audit.logger import Operator
from fleetfix.screens.confirm import ConfirmModal, ConfirmRequest


class _Harness(App[None]):
    """Minimal app that pushes a ConfirmModal and captures its result."""

    def __init__(self, request: ConfirmRequest) -> None:
        super().__init__()
        self._request = request
        self.result: bool | None = None

    def compose(self) -> ComposeResult:
        yield Static("host")

    def on_mount(self) -> None:
        self.push_screen(ConfirmModal(self._request), self._capture)

    def _capture(self, result: bool | None) -> None:
        self.result = result


def _request(phrase: str = "DELETE") -> ConfirmRequest:
    return ConfirmRequest(
        title="Delete /home/operator/old.sql",
        description="Will permanently remove 1.2 GB. This cannot be undone.",
        expected_phrase=phrase,
        operator=Operator(unix_user="operator", auth_principal=None, source_ip="10.1.2.3"),
    )


@pytest.mark.asyncio
async def test_confirm_returns_true_when_phrase_matches() -> None:
    app = _Harness(_request())
    async with app.run_test() as pilot:
        await pilot.pause()
        await pilot.press(*"DELETE")
        await pilot.press("enter")
        await pilot.pause()
    assert app.result is True


@pytest.mark.asyncio
async def test_confirm_rejects_wrong_phrase_then_cancels() -> None:
    app = _Harness(_request())
    async with app.run_test() as pilot:
        await pilot.pause()
        await pilot.press(*"WRONG")
        await pilot.press("enter")
        await pilot.pause()
        # error label shows
        from fleetfix.screens.confirm import ConfirmModal as _CM

        modal = app.screen
        assert isinstance(modal, _CM)
        assert modal.query_one("#confirm-error").display is True
        await pilot.press("escape")
        await pilot.pause()
    assert app.result is False


@pytest.mark.asyncio
async def test_confirm_cancels_on_escape_immediately() -> None:
    app = _Harness(_request())
    async with app.run_test() as pilot:
        await pilot.pause()
        await pilot.press("escape")
        await pilot.pause()
    assert app.result is False


@pytest.mark.asyncio
async def test_submit_button_disabled_until_phrase_matches() -> None:
    app = _Harness(_request())
    async with app.run_test() as pilot:
        await pilot.pause()
        from textual.widgets import Button

        modal = app.screen
        submit = modal.query_one("#confirm-submit", Button)
        assert submit.disabled is True
        await pilot.press(*"DEL")
        await pilot.pause()
        assert submit.disabled is True
        await pilot.press(*"ETE")
        await pilot.pause()
        assert submit.disabled is False
        await pilot.press("escape")
