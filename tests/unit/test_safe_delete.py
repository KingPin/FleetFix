"""Unit tests for the safe single-file delete primitive."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.modules.storage.safe_delete import (
    BlacklistedPath,
    UnsafeDelete,
    safe_delete,
)


def _logger(tmp_path: Path) -> AuditLogger:
    return AuditLogger(
        tmp_path / "audit.log",
        operator=Operator(unix_user="appuser"),
        host="test-host",
    )


def _records(audit: AuditLogger) -> list[dict[str, object]]:
    return [json.loads(line) for line in audit.path.read_text().splitlines() if line]


def test_safe_delete_removes_file_and_returns_size(tmp_path: Path) -> None:
    target = tmp_path / "junk.sql"
    target.write_bytes(b"x" * 4096)
    audit = _logger(tmp_path)

    result = safe_delete(target, audit)

    assert result.bytes_freed == 4096
    assert result.path == target
    assert not target.exists()


def test_safe_delete_writes_paired_intent_and_result(tmp_path: Path) -> None:
    target = tmp_path / "ok.dump"
    target.write_bytes(b"y" * 100)
    audit = _logger(tmp_path)

    safe_delete(target, audit)

    records = _records(audit)
    assert len(records) == 2
    intent, result = records
    assert intent["phase"] == "intent"
    assert intent["action"] == "storage.delete_file"
    assert intent["target"] == {"path": str(target)}
    assert result["phase"] == "result"
    assert result["result"]["ok"] is True
    assert result["result"]["bytes_freed"] == 100
    assert intent["call_id"] == result["call_id"]


def test_safe_delete_refuses_blacklisted_path_before_audit(tmp_path: Path) -> None:
    audit = _logger(tmp_path)
    with pytest.raises(BlacklistedPath):
        safe_delete(Path("/etc/passwd"), audit)
    # Nothing should have been written — the guard sits before audit.action()
    assert _records(audit) == []


def test_safe_delete_refuses_directory(tmp_path: Path) -> None:
    target = tmp_path / "subdir"
    target.mkdir()
    audit = _logger(tmp_path)

    with pytest.raises(UnsafeDelete, match="directory"):
        safe_delete(target, audit)

    records = _records(audit)
    assert [r["phase"] for r in records] == ["intent", "result"]
    assert records[1]["result"]["ok"] is False
    assert "UnsafeDelete" in records[1]["result"]["error"]
    assert target.exists()


def test_safe_delete_refuses_symlink(tmp_path: Path) -> None:
    real = tmp_path / "real.txt"
    real.write_text("hello")
    link = tmp_path / "link.txt"
    link.symlink_to(real)
    audit = _logger(tmp_path)

    with pytest.raises(UnsafeDelete, match="symlink"):
        safe_delete(link, audit)
    assert link.exists()
    assert real.exists()


def test_safe_delete_missing_file_is_audited_as_failure(tmp_path: Path) -> None:
    audit = _logger(tmp_path)

    with pytest.raises(FileNotFoundError):
        safe_delete(tmp_path / "ghost.sql", audit)

    records = _records(audit)
    assert len(records) == 2
    assert records[1]["result"]["ok"] is False
    assert "FileNotFoundError" in records[1]["result"]["error"]
