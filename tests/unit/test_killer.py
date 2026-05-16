"""Tests for the audit-wrapped process killer."""

from __future__ import annotations

import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import pytest

from fleetfix.audit.logger import AuditLogger, Operator
from fleetfix.modules.procs.killer import (
    PROTECTED_PIDS,
    force_kill,
    send_signal,
    terminate,
)


@pytest.fixture
def audit(tmp_path: Path) -> AuditLogger:
    return AuditLogger(
        tmp_path / "audit.log",
        operator=Operator(unix_user="test"),
    )


def _read_audit(path: Path) -> list[dict]:  # type: ignore[type-arg]
    return [json.loads(line) for line in path.read_text().splitlines() if line.strip()]


@pytest.mark.parametrize("pid", PROTECTED_PIDS)
def test_refuses_protected_pids(pid: int, audit: AuditLogger) -> None:
    result = send_signal(pid, sig=signal.SIGTERM, audit=audit)
    assert not result.ok
    assert "protected" in (result.error or "")
    # Refusal must NOT write an audit record — we only audit attempts that
    # leave the safety gate.
    assert _read_audit(audit.path) == []


def test_nonexistent_pid_returns_error(audit: AuditLogger) -> None:
    # PID 2^31 - 1 is the kernel max; vanishingly unlikely to exist.
    pid = 2**31 - 1
    result = send_signal(pid, sig=signal.SIGTERM, audit=audit)
    assert not result.ok
    assert "no such process" in (result.error or "")
    # The pid is included so an operator reading the message knows which
    # process the tool tried to signal.
    assert str(pid) in (result.error or "")
    lines = _read_audit(audit.path)
    # intent + result both written; result records ESRCH.
    assert [line["phase"] for line in lines] == ["intent", "result"]
    assert lines[1]["result"]["errno"] == "ESRCH"
    assert lines[1]["result"]["ok"] is True
    # ok=True at the audit layer means "the wrapped block didn't raise";
    # the KillResult.ok=False at the caller layer is what the operator sees.


def test_terminate_signals_a_real_subprocess(audit: AuditLogger) -> None:
    # Spawn a python child that sleeps long enough for us to signal it.
    child = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)"])
    try:
        # Give it a moment to land in the kernel.
        time.sleep(0.05)
        result = terminate(child.pid, audit=audit)
        assert result.ok, result.error
        # Reap; SIGTERM exit code on python is 143 (128+15) but exact value
        # isn't load-bearing for the test.
        child.wait(timeout=2)
    finally:
        if child.poll() is None:
            child.kill()
            child.wait()

    lines = _read_audit(audit.path)
    assert len(lines) == 2
    intent, result_line = lines
    assert intent["phase"] == "intent"
    assert intent["action"] == "procs.signal"
    assert intent["target"]["pid"] == child.pid
    assert intent["target"]["signal_name"] == "SIGTERM"
    assert result_line["result"]["ok"] is True


def test_force_kill_uses_sigkill(monkeypatch: pytest.MonkeyPatch, audit: AuditLogger) -> None:
    sent: dict[str, int] = {}

    def fake_kill(pid: int, sig: int) -> None:
        sent["pid"] = pid
        sent["sig"] = sig

    monkeypatch.setattr(os, "kill", fake_kill)
    result = force_kill(99999, audit=audit)
    assert result.ok
    assert sent == {"pid": 99999, "sig": signal.SIGKILL}


def test_permission_error_is_surfaced(monkeypatch: pytest.MonkeyPatch, audit: AuditLogger) -> None:
    def fake_kill(_pid: int, _sig: int) -> None:
        raise PermissionError("operation not permitted")

    monkeypatch.setattr(os, "kill", fake_kill)
    result = send_signal(99999, sig=signal.SIGTERM, audit=audit)
    assert not result.ok
    error_msg = result.error or ""
    assert "permission denied" in error_msg
    # The fix hint nudges the operator toward sudo escalation.
    assert "sudo" in error_msg
    lines = _read_audit(audit.path)
    assert lines[-1]["result"]["errno"] == "EPERM"
