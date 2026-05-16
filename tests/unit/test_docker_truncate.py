"""Unit tests for docker.truncate — log path resolve + audit pairing."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.modules.docker import truncate as truncate_mod


@pytest.fixture
def audit(tmp_path: Path) -> AuditLogger:
    return AuditLogger(
        tmp_path / "audit.log",
        operator=Operator(unix_user="tester"),
    )


def _read_audit(audit: AuditLogger) -> list[dict]:
    return [json.loads(line) for line in audit.path.read_text().splitlines() if line.strip()]


def test_truncate_log_happy_path_records_bytes_freed(
    tmp_path: Path,
    audit: AuditLogger,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    log_file = tmp_path / "abc-json.log"
    log_file.write_bytes(b"x" * 1234)

    monkeypatch.setattr(truncate_mod, "resolve_log_path", lambda cid: str(log_file))

    def fake_run(path: str) -> None:
        # Simulate `truncate -s 0` by writing empty bytes.
        Path(path).write_bytes(b"")
        return None

    monkeypatch.setattr(truncate_mod, "_run_truncate", fake_run)

    result = truncate_mod.truncate_log("abc", audit=audit, container_name="web")

    assert result.ok is True
    assert result.bytes_freed == 1234
    records = _read_audit(audit)
    intent = next(r for r in records if r["phase"] == "intent")
    res = next(r for r in records if r["phase"] == "result")
    assert intent["action"] == "docker.truncate_log"
    assert intent["target"]["size_before"] == 1234
    assert res["result"]["ok"] is True
    assert res["result"]["bytes_freed"] == 1234


def test_truncate_log_reports_failure_in_audit(
    tmp_path: Path,
    audit: AuditLogger,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    log_file = tmp_path / "abc-json.log"
    log_file.write_bytes(b"x" * 100)
    monkeypatch.setattr(truncate_mod, "resolve_log_path", lambda cid: str(log_file))
    monkeypatch.setattr(truncate_mod, "_run_truncate", lambda p: "permission denied")

    result = truncate_mod.truncate_log("abc", audit=audit)

    assert result.ok is False
    assert result.error == "permission denied"
    records = _read_audit(audit)
    res = next(r for r in records if r["phase"] == "result")
    assert res["result"]["ok"] is False
    assert res["result"]["error"] == "permission denied"


def test_truncate_log_refuses_when_path_unknown(
    audit: AuditLogger,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    monkeypatch.setattr(truncate_mod, "resolve_log_path", lambda cid: None)

    result = truncate_mod.truncate_log("nope", audit=audit)

    assert result.ok is False
    assert result.error == "could not resolve container log path"
    # No audit lines should have been written for an action we never attempted.
    assert _read_audit(audit) == []
