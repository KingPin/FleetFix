"""Unit tests for the apt update status readout."""

from __future__ import annotations

import subprocess
from pathlib import Path
from typing import Any

import pytest

from fleetfix.modules.system import updates
from fleetfix.modules.system.updates import (
    from_apt,
    from_notifier,
    get_update_status,
    parse_notifier_text,
)

_NOTIFIER_REGULAR = """\

14 packages can be updated.
8 updates are security updates.

"""

_NOTIFIER_NO_SECURITY = """\
3 packages can be updated.
"""


def test_parse_notifier_regular() -> None:
    parsed = parse_notifier_text(_NOTIFIER_REGULAR)
    assert parsed == (14, 8)


def test_parse_notifier_without_security_line() -> None:
    parsed = parse_notifier_text(_NOTIFIER_NO_SECURITY)
    assert parsed == (3, 0)


def test_parse_notifier_unrecognised() -> None:
    assert parse_notifier_text("nothing to see here\n") is None


def test_from_notifier_reads_existing_file(tmp_path: Path) -> None:
    f = tmp_path / "updates-available"
    f.write_text(_NOTIFIER_REGULAR)
    status = from_notifier(f)
    assert status is not None
    assert status.upgradable == 14
    assert status.security == 8
    assert status.source == "update-notifier"


def test_from_notifier_missing_file(tmp_path: Path) -> None:
    assert from_notifier(tmp_path / "absent") is None


def test_from_apt_counts_lines(monkeypatch: pytest.MonkeyPatch) -> None:
    sample = (
        "Listing... Done\n"
        "curl/jammy-updates 8.0.1-1 amd64 [upgradable from: 8.0.0-1]\n"
        "openssl/jammy-security 3.0.2-0ubuntu1.10 amd64 [upgradable from: 3.0.2-0ubuntu1.9]\n"
        "vim/jammy-updates 9.0-1 amd64 [upgradable from: 8.2-1]\n"
    )

    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args=args, returncode=0, stdout=sample)

    monkeypatch.setattr(updates.subprocess, "run", fake_run)
    status = from_apt()
    assert status is not None
    assert status.upgradable == 3
    assert status.security == 1
    assert status.source == "apt"


def test_from_apt_handles_missing_binary(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise FileNotFoundError("no apt")

    monkeypatch.setattr(updates.subprocess, "run", boom)
    assert from_apt() is None


def test_from_apt_handles_nonzero(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args=args, returncode=100, stdout="", stderr="err")

    monkeypatch.setattr(updates.subprocess, "run", fake_run)
    assert from_apt() is None


def test_get_update_status_prefers_notifier(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    notif = tmp_path / "updates-available"
    notif.write_text(_NOTIFIER_REGULAR)
    monkeypatch.setattr(updates, "NOTIFIER_PATH", notif)

    def should_not_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise AssertionError("apt fallback should not run when notifier is present")

    monkeypatch.setattr(updates.subprocess, "run", should_not_run)
    status = get_update_status()
    assert status.source == "update-notifier"
    assert status.upgradable == 14


def test_get_update_status_falls_back_to_apt(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(updates, "NOTIFIER_PATH", tmp_path / "absent")

    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(
            args=args,
            returncode=0,
            stdout="Listing... Done\nfoo/x 1.0 amd64 [upgradable from: 0.9]\n",
        )

    monkeypatch.setattr(updates.subprocess, "run", fake_run)
    status = get_update_status()
    assert status.source == "apt"
    assert status.upgradable == 1


def test_get_update_status_returns_unavailable_when_all_fail(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch
) -> None:
    monkeypatch.setattr(updates, "NOTIFIER_PATH", tmp_path / "absent")

    def boom(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise FileNotFoundError("no apt")

    monkeypatch.setattr(updates.subprocess, "run", boom)
    status = get_update_status()
    assert status.source == "unavailable"
    assert status.upgradable == 0
    assert status.error is not None
