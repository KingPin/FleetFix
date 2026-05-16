"""Unit tests for services.journal — argv shape, failure modes."""

from __future__ import annotations

import subprocess

import pytest

from fleetfix.modules.services.journal import journal_tail


class _CompletedProcess:
    def __init__(self, stdout: str = "", stderr: str = "", returncode: int = 0) -> None:
        self.stdout = stdout
        self.stderr = stderr
        self.returncode = returncode


def test_journal_tail_returns_output(monkeypatch: pytest.MonkeyPatch) -> None:
    captured: dict[str, list[str]] = {}

    def fake_run(argv, **kwargs):  # type: ignore[no-untyped-def]
        captured["argv"] = argv
        return _CompletedProcess(stdout="line1\nline2\n")

    monkeypatch.setattr(subprocess, "run", fake_run)
    out = journal_tail("kafka.service", lines=50)
    assert out == "line1\nline2\n"
    assert captured["argv"][:5] == ["journalctl", "-u", "kafka.service", "-n", "50"]


def test_journal_tail_handles_missing_binary(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(argv, **kwargs):  # type: ignore[no-untyped-def]
        raise FileNotFoundError

    monkeypatch.setattr(subprocess, "run", fake_run)
    assert "not found" in journal_tail("any.service")


def test_journal_tail_handles_timeout(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(argv, **kwargs):  # type: ignore[no-untyped-def]
        raise subprocess.TimeoutExpired(cmd=argv, timeout=10)

    monkeypatch.setattr(subprocess, "run", fake_run)
    assert "timed out" in journal_tail("any.service")


def test_journal_tail_surfaces_nonzero_exit(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(argv, **kwargs):  # type: ignore[no-untyped-def]
        return _CompletedProcess(stderr="No entries.", returncode=1)

    monkeypatch.setattr(subprocess, "run", fake_run)
    assert "No entries." == journal_tail("any.service")


def test_journal_tail_empty_unit_returns_placeholder() -> None:
    assert "no unit" in journal_tail("")
