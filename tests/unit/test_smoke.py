"""Smoke tests — verify core modules import and basic invariants hold."""

from __future__ import annotations

import re

import fleetfix
from fleetfix import app, config, privilege


def test_version_is_semver() -> None:
    assert re.match(r"^\d+\.\d+\.\d+", fleetfix.__version__)


def test_host_detect_returns_populated_record() -> None:
    info = config.detect_host()
    assert info.hostname
    assert info.kernel
    assert info.arch


def test_privilege_detect_is_consistent() -> None:
    state = privilege.detect()
    # passwordless implies sudo_available
    if state.passwordless_sudo:
        assert state.sudo_available
    # root implies all three
    if state.is_root:
        assert state.sudo_available and state.passwordless_sudo


def test_app_constructs_without_running() -> None:
    instance = app.FleetFixApp(read_only=True)
    assert instance.ctx.version == fleetfix.__version__
    assert instance.ctx.read_only is True
    assert instance.host.hostname
