"""E2E test fixtures — neutralize external services that the App reaches for."""

from __future__ import annotations

import pytest


@pytest.fixture(autouse=True)
def _no_update_check(monkeypatch: pytest.MonkeyPatch) -> None:
    """Prevent the launch-time GitHub release check from firing in pilot tests."""
    monkeypatch.setattr("fleetfix.app.check_for_update", lambda *a, **kw: None)
