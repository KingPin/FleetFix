"""Unit tests for privilege detection + sudo keepalive helpers.

We never invoke real sudo in tests — subprocess.run is stubbed so the
suite stays safe and deterministic regardless of host configuration.
"""

from __future__ import annotations

import subprocess
from typing import Any

import pytest

from fleetfix import privilege


@pytest.fixture(autouse=True)
def _no_real_sudo(monkeypatch: pytest.MonkeyPatch) -> None:
    """Fail loudly if any test forgets to stub subprocess.run for sudo."""

    def _trap(*args: Any, **kwargs: Any) -> None:  # pragma: no cover
        raise RuntimeError("subprocess.run should be patched in privilege tests")

    monkeypatch.setattr(subprocess, "run", _trap)


def _stub_run(returncode: int = 0, *, timeout: bool = False, oserror: bool = False) -> Any:
    """Build a subprocess.run replacement matching the given outcome."""
    completed = subprocess.CompletedProcess(args=[], returncode=returncode)

    def fake(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        if timeout:
            raise subprocess.TimeoutExpired(cmd=args[0], timeout=kwargs.get("timeout", 0))
        if oserror:
            raise OSError("sudo binary missing")
        return completed

    return fake


def test_detect_running_as_root(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("os.geteuid", lambda: 0)
    state = privilege.detect()
    assert state.is_root is True
    assert state.sudo_available is True
    assert state.passwordless_sudo is True
    assert state.can_tier2 is True


def test_detect_no_sudo_binary(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("os.geteuid", lambda: 1000)
    monkeypatch.setattr("shutil.which", lambda _cmd: None)
    state = privilege.detect()
    assert state.is_root is False
    assert state.sudo_available is False
    assert state.passwordless_sudo is False
    assert state.can_tier2 is False


def test_detect_passwordless_sudo_succeeds(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("os.geteuid", lambda: 1000)
    monkeypatch.setattr("shutil.which", lambda _cmd: "/usr/bin/sudo")
    monkeypatch.setattr(subprocess, "run", _stub_run(returncode=0))
    state = privilege.detect()
    assert state.is_root is False
    assert state.sudo_available is True
    assert state.passwordless_sudo is True


def test_detect_sudo_present_but_password_required(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr("os.geteuid", lambda: 1000)
    monkeypatch.setattr("shutil.which", lambda _cmd: "/usr/bin/sudo")
    monkeypatch.setattr(subprocess, "run", _stub_run(returncode=1))
    state = privilege.detect()
    assert state.is_root is False
    assert state.sudo_available is True
    assert state.passwordless_sudo is False
    assert state.can_tier2 is True  # password may still be cached


def test_passwordless_check_handles_timeout(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(subprocess, "run", _stub_run(timeout=True))
    assert privilege._check_passwordless_sudo() is False


def test_passwordless_check_handles_oserror(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(subprocess, "run", _stub_run(oserror=True))
    assert privilege._check_passwordless_sudo() is False


def test_refresh_sudo_credential_quietly_success(monkeypatch: pytest.MonkeyPatch) -> None:
    captured: dict[str, Any] = {}

    def capture(args: list[str], **kwargs: Any) -> subprocess.CompletedProcess:
        captured["args"] = args
        captured["timeout"] = kwargs.get("timeout")
        return subprocess.CompletedProcess(args=args, returncode=0)

    monkeypatch.setattr(subprocess, "run", capture)
    assert privilege.refresh_sudo_credential_quietly() is True
    assert captured["args"] == ["sudo", "-n", "-v"]
    assert captured["timeout"] == 2


def test_refresh_sudo_credential_quietly_failure(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(subprocess, "run", _stub_run(returncode=1))
    assert privilege.refresh_sudo_credential_quietly() is False


def test_keepalive_interval_under_sudo_default() -> None:
    # sudo's timestamp_timeout default is 300s; ours must fire before that.
    assert privilege.SUDO_KEEPALIVE_INTERVAL < 300


def test_refresh_sudo_credential_interactive(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(subprocess, "run", _stub_run(returncode=0))
    assert privilege.refresh_sudo_credential() is True


def test_refresh_sudo_credential_handles_oserror(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(subprocess, "run", _stub_run(oserror=True))
    assert privilege.refresh_sudo_credential() is False
