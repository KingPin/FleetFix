"""Unit tests for the ss listening-socket parser."""

from __future__ import annotations

import subprocess
from typing import Any

import pytest

from fleetfix.modules.network import sockets
from fleetfix.modules.network.sockets import list_listening_sockets, parse_ss_output
from tests.support.fixtures import fixture

_SS_OUTPUT = fixture("ss/listen_no_header.txt")

_SS_WITH_HEADER = fixture("ss/listen_with_header.txt")


def test_parse_simple_v4_listener() -> None:
    sockets_list = parse_ss_output(_SS_OUTPUT)
    pg = next(s for s in sockets_list if s.local_port == 5432)
    assert pg.local_address == "127.0.0.1"
    assert pg.process_name == "postgres"
    assert pg.pid == 1234


def test_parse_dual_stack_ipv6() -> None:
    sockets_list = parse_ss_output(_SS_OUTPUT)
    v6_ssh = [s for s in sockets_list if s.local_port == 22 and s.local_address == "::"]
    assert len(v6_ssh) == 1
    assert v6_ssh[0].process_name == "sshd"


def test_parse_with_header_row() -> None:
    parsed = parse_ss_output(_SS_WITH_HEADER)
    assert len(parsed) == 1
    assert parsed[0].process_name == "postgres"


def test_parse_drops_non_listen_lines() -> None:
    mixed = fixture("ss/estab_only.txt") + _SS_OUTPUT
    parsed = parse_ss_output(mixed)
    assert all(s.local_port for s in parsed)
    assert 443 not in {s.local_port for s in parsed}


def test_parse_handles_missing_users_field() -> None:
    parsed = parse_ss_output(fixture("ss/listen_no_users.txt"))
    assert len(parsed) == 1
    assert parsed[0].local_port == 9999
    assert parsed[0].process_name is None
    assert parsed[0].pid is None


def test_parse_empty_input() -> None:
    assert parse_ss_output("") == []


def test_list_listening_sockets_invokes_ss(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args=args, returncode=0, stdout=_SS_OUTPUT)

    monkeypatch.setattr(sockets.subprocess, "run", fake_run)
    parsed = list_listening_sockets()
    assert any(s.process_name == "postgres" for s in parsed)


def test_list_listening_sockets_returns_empty_on_failure(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        raise FileNotFoundError("no ss binary")

    monkeypatch.setattr(sockets.subprocess, "run", boom)
    assert list_listening_sockets() == []


def test_list_listening_sockets_returns_empty_on_nonzero(monkeypatch: pytest.MonkeyPatch) -> None:
    def fake_run(*args: Any, **kwargs: Any) -> subprocess.CompletedProcess:
        return subprocess.CompletedProcess(args=args, returncode=2, stdout="", stderr="err")

    monkeypatch.setattr(sockets.subprocess, "run", fake_run)
    assert list_listening_sockets() == []
