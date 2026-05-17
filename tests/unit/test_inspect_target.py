"""Unit tests for InspectTarget resolution."""

from __future__ import annotations

import logging
import os
from pathlib import Path

import pytest

from fleetfix.config import InspectTarget, _read_paths_yaml, resolve_inspect_target


@pytest.fixture
def current_user() -> str:
    """A username guaranteed to exist on the test host (the invoking user)."""
    import pwd

    return pwd.getpwuid(os.getuid()).pw_name


def test_resolve_returns_none_when_unset() -> None:
    assert resolve_inspect_target(cli_user=None, paths_cfg={}) is None


def test_resolve_returns_inspect_target_for_existing_cli_user(current_user: str) -> None:
    target = resolve_inspect_target(cli_user=current_user, paths_cfg={})
    assert isinstance(target, InspectTarget)
    assert target.user == current_user
    assert target.home.exists()
    assert isinstance(target.uid, int)


def test_resolve_reads_target_from_paths_cfg(current_user: str) -> None:
    target = resolve_inspect_target(cli_user=None, paths_cfg={"target_user": current_user})
    assert target is not None
    assert target.user == current_user


def test_cli_overrides_paths_cfg(current_user: str) -> None:
    # CLI names a real user; paths_cfg names a likely-bogus one.
    target = resolve_inspect_target(
        cli_user=current_user, paths_cfg={"target_user": "no-such-user-xyzzy"}
    )
    assert target is not None
    assert target.user == current_user


def test_empty_cli_string_clears_paths_cfg(current_user: str) -> None:
    target = resolve_inspect_target(cli_user="", paths_cfg={"target_user": current_user})
    assert target is None


def test_unknown_user_returns_none_and_warns(
    caplog: pytest.LogCaptureFixture,
) -> None:
    caplog.set_level(logging.WARNING)
    target = resolve_inspect_target(cli_user="no-such-user-xyzzy", paths_cfg={})
    assert target is None
    assert any("no-such-user-xyzzy" in r.message for r in caplog.records)


def test_read_paths_yaml_missing_file_returns_empty(tmp_path: Path) -> None:
    assert _read_paths_yaml(tmp_path / "absent.yml") == {}


def test_read_paths_yaml_parses_mapping(tmp_path: Path) -> None:
    p = tmp_path / "paths.yml"
    p.write_text("target_user: appuser\nstale_age_days: 30\n", encoding="utf-8")
    out = _read_paths_yaml(p)
    assert out == {"target_user": "appuser", "stale_age_days": 30}


def test_read_paths_yaml_non_mapping_returns_empty(tmp_path: Path) -> None:
    p = tmp_path / "paths.yml"
    p.write_text("- just\n- a\n- list\n", encoding="utf-8")
    assert _read_paths_yaml(p) == {}


def test_read_paths_yaml_malformed_returns_empty(tmp_path: Path) -> None:
    p = tmp_path / "paths.yml"
    p.write_text("target_user: [unterminated\n", encoding="utf-8")
    assert _read_paths_yaml(p) == {}
