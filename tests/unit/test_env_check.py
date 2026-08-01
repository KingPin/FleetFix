"""Unit tests for the .env / config validator."""

from __future__ import annotations

from pathlib import Path

from fleetfix.modules.storage.env_check import check_env_file
from tests.support.fixtures import fixture


def test_missing_file_lists_required_keys(tmp_path: Path) -> None:
    result = check_env_file(tmp_path / "absent.env", required_keys=["DB_URL", "API_KEY"])
    assert result.exists is False
    assert result.readable is False
    assert result.missing_required == ["DB_URL", "API_KEY"]
    assert result.ok is False


def test_well_formed_env_parses_all_keys(tmp_path: Path) -> None:
    path = tmp_path / "good.env"
    path.write_text(fixture("dotenv/well_formed.txt"))
    result = check_env_file(path, required_keys=["DB_URL", "API_KEY", "PORT"])
    assert result.exists and result.readable
    assert result.keys["DB_URL"] == "postgres://localhost"
    assert result.keys["API_KEY"] == "s3cret"
    assert result.keys["DEBUG"] == "true"
    assert result.keys["PORT"] == "5432"
    assert result.missing_required == []
    assert result.issues == []
    assert result.ok is True


def test_required_key_absence_reported(tmp_path: Path) -> None:
    path = tmp_path / "partial.env"
    path.write_text(fixture("dotenv/single_key.txt"))
    result = check_env_file(path, required_keys=["FOO", "MISSING"])
    assert result.missing_required == ["MISSING"]
    assert result.ok is False


def test_malformed_line_becomes_issue_but_parsing_continues(tmp_path: Path) -> None:
    path = tmp_path / "broken.env"
    path.write_text(fixture("dotenv/malformed_line.txt"))
    result = check_env_file(path)
    assert result.keys["GOOD"] == "ok"
    assert result.keys["ALSO_GOOD"] == "also-ok"
    assert len(result.issues) == 1
    assert result.issues[0].line_no == 2
    assert "KEY=value" in result.issues[0].message
    assert result.ok is False


def test_duplicate_keys_reported(tmp_path: Path) -> None:
    path = tmp_path / "dup.env"
    path.write_text(fixture("dotenv/duplicate_keys.txt"))
    result = check_env_file(path)
    assert result.keys["X"] == "2"
    assert any("duplicate" in i.message for i in result.issues)


def test_inline_comment_stripped(tmp_path: Path) -> None:
    path = tmp_path / "inline.env"
    path.write_text(fixture("dotenv/inline_comment.txt"))
    result = check_env_file(path)
    assert result.keys["KEY"] == "value"


def test_empty_value_is_allowed(tmp_path: Path) -> None:
    path = tmp_path / "empty.env"
    path.write_text(fixture("dotenv/empty_value.txt"))
    result = check_env_file(path)
    assert result.keys["EMPTY"] == ""
    assert result.ok is True
