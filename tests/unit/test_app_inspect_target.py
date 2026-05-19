"""Unit tests for FleetFixApp inspect_target wiring."""

from __future__ import annotations

import os
import pwd

import pytest

from fleetfix.app import FleetFixApp
from fleetfix.config import InspectTarget


@pytest.fixture
def current_user() -> str:
    return pwd.getpwuid(os.getuid()).pw_name


def test_app_default_has_no_inspect_target() -> None:
    app = FleetFixApp(check_for_update_on_mount=False)
    assert app.inspect_target is None
    assert app.audit.inspect_target is None


def test_app_with_target_user_resolves_target(current_user: str) -> None:
    app = FleetFixApp(check_for_update_on_mount=False, target_user=current_user)
    assert isinstance(app.inspect_target, InspectTarget)
    assert app.inspect_target.user == current_user
    assert app.audit.inspect_target == current_user


def test_app_with_bogus_target_user_warns_and_clears() -> None:
    app = FleetFixApp(check_for_update_on_mount=False, target_user="no-such-user-xyzzy")
    assert app.inspect_target is None
    assert app.audit.inspect_target is None
