"""Tests for the JSON-lines audit logger."""

from __future__ import annotations

import json
from pathlib import Path

import pytest

from fleetfix.audit.logger import AuditLogger, Operator, _parse_ssh_source_ip, read_recent


@pytest.fixture
def operator() -> Operator:
    return Operator(unix_user="operator", auth_principal=None, source_ip="10.1.2.3")


@pytest.fixture
def logger(tmp_path: Path, operator: Operator) -> AuditLogger:
    return AuditLogger(
        tmp_path / "audit.log",
        operator=operator,
        host="test-host",
        session_id="11111111-1111-1111-1111-111111111111",
    )


def _read_lines(path: Path) -> list[dict]:
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


def test_event_writes_single_line(logger: AuditLogger) -> None:
    logger.event("fleetfix.launch", build="0.1.0")
    records = _read_lines(logger.path)
    assert len(records) == 1
    assert records[0]["action"] == "fleetfix.launch"
    assert records[0]["phase"] == "event"
    assert records[0]["target"]["build"] == "0.1.0"
    assert records[0]["seq"] == 1


def test_action_pairs_intent_and_result(logger: AuditLogger) -> None:
    with logger.action("storage.delete", target={"path": "/tmp/x"}) as call:
        call.set_result(bytes_freed=1024)

    records = _read_lines(logger.path)
    assert len(records) == 2
    intent, result = records
    assert intent["phase"] == "intent"
    assert intent["result"] is None
    assert result["phase"] == "result"
    assert result["result"]["ok"] is True
    assert result["result"]["error"] is None
    assert result["result"]["bytes_freed"] == 1024
    assert intent["call_id"] == result["call_id"]
    assert intent["seq"] < result["seq"]


def test_action_records_failure_when_block_raises(logger: AuditLogger) -> None:
    with pytest.raises(RuntimeError):
        with logger.action("storage.delete", target={"path": "/tmp/x"}):
            raise RuntimeError("permission denied")

    records = _read_lines(logger.path)
    assert len(records) == 2
    assert records[0]["phase"] == "intent"
    assert records[1]["result"]["ok"] is False
    assert "RuntimeError" in records[1]["result"]["error"]
    assert "permission denied" in records[1]["result"]["error"]


def test_seq_monotonic_across_calls(logger: AuditLogger) -> None:
    logger.event("a")
    logger.event("b")
    with logger.action("c"):
        pass
    records = _read_lines(logger.path)
    seqs = [r["seq"] for r in records]
    assert seqs == sorted(seqs)
    assert len(set(seqs)) == len(seqs)


def test_session_id_constant_across_records(logger: AuditLogger) -> None:
    logger.event("a")
    with logger.action("b"):
        pass
    records = _read_lines(logger.path)
    session_ids = {r["session_id"] for r in records}
    assert len(session_ids) == 1


def test_writes_each_field_correctly(logger: AuditLogger) -> None:
    with logger.action("docker.truncate_log", target={"container_id": "abc"}):
        pass
    intent = _read_lines(logger.path)[0]
    assert intent["host"] == "test-host"
    assert intent["operator"]["unix_user"] == "operator"
    assert intent["operator"]["source_ip"] == "10.1.2.3"
    assert intent["operator"]["auth_principal"] is None
    assert intent["action"] == "docker.truncate_log"
    assert intent["target"]["container_id"] == "abc"
    assert intent["fleetfix_version"]


def test_read_recent_returns_parsed_records_in_order(logger: AuditLogger) -> None:
    for i in range(5):
        logger.event(f"event-{i}")
    out = read_recent(logger.path, limit=3)
    assert len(out) == 3
    assert [r["action"] for r in out] == ["event-2", "event-3", "event-4"]


def test_read_recent_skips_malformed_lines(tmp_path: Path) -> None:
    path = tmp_path / "audit.log"
    path.write_text('{"ok": true}\nnot-json\n{"ok": false}\n')
    out = read_recent(path)
    assert [r["ok"] for r in out] == [True, False]


def test_read_recent_returns_empty_for_missing_file(tmp_path: Path) -> None:
    assert read_recent(tmp_path / "nope.log") == []


@pytest.mark.parametrize(
    "env,expected",
    [
        (None, None),
        ("", None),
        ("10.0.0.5 54321 192.168.1.1 22", "10.0.0.5"),
        ("::1 54321 ::1 22", "::1"),
    ],
)
def test_parse_ssh_source_ip(env: str | None, expected: str | None) -> None:
    assert _parse_ssh_source_ip(env) == expected


def test_operator_from_environment_uses_sudo_user_first(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setenv("USER", "root")
    monkeypatch.setenv("SUDO_USER", "alice")
    monkeypatch.setenv("SSH_CONNECTION", "10.0.0.5 22 192.168.1.1 22")
    op = Operator.from_environment()
    assert op.unix_user == "alice"
    assert op.source_ip == "10.0.0.5"


def test_operator_falls_back_to_unknown(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.delenv("USER", raising=False)
    monkeypatch.delenv("SUDO_USER", raising=False)
    monkeypatch.delenv("SSH_CONNECTION", raising=False)
    op = Operator.from_environment()
    assert op.unix_user == "unknown"
    assert op.source_ip is None


def test_sink_receives_each_record(tmp_path: Path, operator: Operator) -> None:
    received: list[dict] = []
    logger = AuditLogger(
        tmp_path / "audit.log",
        operator=operator,
        sink=received.append,
    )
    logger.event("fleetfix.launch")
    with logger.action("storage.delete", target={"path": "/tmp/x"}):
        pass
    assert [r["phase"] for r in received] == ["event", "intent", "result"]
    # Local file mirrors the same records.
    assert len(_read_lines(logger.path)) == 3


def test_sink_exception_does_not_break_local_logging(tmp_path: Path, operator: Operator) -> None:
    def explode(record: dict) -> None:
        raise RuntimeError("downstream broken")

    logger = AuditLogger(tmp_path / "audit.log", operator=operator, sink=explode)
    # Must not raise — sink errors are swallowed, local file stays authoritative.
    logger.event("fleetfix.launch")
    with logger.action("docker.truncate_log", target={"container_id": "abc"}):
        pass
    records = _read_lines(logger.path)
    assert len(records) == 3
