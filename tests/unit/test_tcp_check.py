"""Tests for outbound TCP reachability checks."""

from __future__ import annotations

import errno
import socket
from typing import Any

import pytest

from fleetfix.modules.network import tcp
from fleetfix.modules.network.tcp import TcpTarget, check_port, parse_host_port


@pytest.mark.parametrize(
    ("raw", "expected"),
    [
        ("db.internal:5432", TcpTarget("db.internal", 5432)),
        ("10.0.0.5:5432", TcpTarget("10.0.0.5", 5432)),
        ("[::1]:5432", TcpTarget("::1", 5432)),
        ("  db.internal:5432  ", TcpTarget("db.internal", 5432)),
        ("https://api.internal/health", TcpTarget("api.internal", 443)),
        ("http://api.internal", TcpTarget("api.internal", 80)),
        ("https://api.internal:8443/health", TcpTarget("api.internal", 8443)),
        ("postgres://db.internal", TcpTarget("db.internal", 5432)),
    ],
)
def test_parse_host_port_accepts(raw: str, expected: TcpTarget) -> None:
    assert parse_host_port(raw) == expected


@pytest.mark.parametrize(
    "raw",
    [
        "",
        "   ",
        "api.internal",  # bare host, no default_port supplied
        "host:notaport",
        "host:99999",
        "host:0",
        "host:-1",
        "[::1",  # unterminated bracket
        "[]:443",
        "::1",  # bare v6 literal: host/port is ambiguous
    ],
)
def test_parse_host_port_rejects(raw: str) -> None:
    assert parse_host_port(raw) is None


def test_parse_host_port_uses_default_port_for_bare_host() -> None:
    assert parse_host_port("api.internal", default_port=443) == TcpTarget("api.internal", 443)


def test_parse_host_port_explicit_port_beats_default() -> None:
    assert parse_host_port("api.internal:9100", default_port=443) == TcpTarget("api.internal", 9100)


def test_target_str_brackets_ipv6() -> None:
    assert str(TcpTarget("::1", 5432)) == "[::1]:5432"
    assert str(TcpTarget("db.internal", 5432)) == "db.internal:5432"
    # Round-trips back through the parser.
    assert parse_host_port(str(TcpTarget("::1", 5432))) == TcpTarget("::1", 5432)


class _FakeSocket:
    def __init__(self, code: int) -> None:
        self._code = code
        self.closed = False
        self.timeout: float | None = None

    def settimeout(self, timeout: float) -> None:
        self.timeout = timeout

    def connect_ex(self, _addr: Any) -> int:
        return self._code

    def close(self) -> None:
        self.closed = True


def _patch_socket(monkeypatch: pytest.MonkeyPatch, code: int) -> list[_FakeSocket]:
    made: list[_FakeSocket] = []

    def fake_getaddrinfo(host: str, port: int, **_kwargs: Any) -> list[Any]:
        return [(socket.AF_INET, socket.SOCK_STREAM, 6, "", (host, port))]

    def fake_socket(*_args: Any, **_kwargs: Any) -> _FakeSocket:
        sock = _FakeSocket(code)
        made.append(sock)
        return sock

    monkeypatch.setattr(tcp.socket, "getaddrinfo", fake_getaddrinfo)
    monkeypatch.setattr(tcp.socket, "socket", fake_socket)
    return made


@pytest.mark.parametrize(
    ("code", "state"),
    [
        (0, "open"),
        (errno.ECONNREFUSED, "refused"),
        (errno.EAGAIN, "timeout"),
        (errno.EINPROGRESS, "timeout"),
        (errno.ETIMEDOUT, "timeout"),
        (errno.EHOSTUNREACH, "unreachable"),
        (errno.ENETUNREACH, "unreachable"),
    ],
)
def test_check_port_maps_errno_to_state(
    monkeypatch: pytest.MonkeyPatch, code: int, state: str
) -> None:
    _patch_socket(monkeypatch, code)
    result = check_port(TcpTarget("db.internal", 5432))
    assert result.state == state
    assert result.ok is (state == "open")
    assert result.error is None


def test_check_port_unknown_errno_is_error(monkeypatch: pytest.MonkeyPatch) -> None:
    _patch_socket(monkeypatch, errno.EACCES)
    result = check_port(TcpTarget("db.internal", 5432))
    assert result.state == "error"
    assert result.error is not None
    assert "EACCES" in result.error


def test_check_port_closes_the_socket(monkeypatch: pytest.MonkeyPatch) -> None:
    made = _patch_socket(monkeypatch, 0)
    check_port(TcpTarget("db.internal", 5432), timeout_s=1.5)
    assert made[0].closed is True
    assert made[0].timeout == 1.5


def test_check_port_reports_dns_error_separately(monkeypatch: pytest.MonkeyPatch) -> None:
    def boom(*_args: Any, **_kwargs: Any) -> list[Any]:
        raise socket.gaierror(-2, "Name or service not known")

    monkeypatch.setattr(tcp.socket, "getaddrinfo", boom)
    result = check_port(TcpTarget("nope.invalid", 443))
    # Layer attribution: a bad name must not look like a filtered port.
    assert result.state == "dns-error"
    assert result.ok is False
    assert result.error is not None


def test_check_port_empty_addrinfo_is_dns_error(monkeypatch: pytest.MonkeyPatch) -> None:
    monkeypatch.setattr(tcp.socket, "getaddrinfo", lambda *a, **k: [])
    result = check_port(TcpTarget("nope.invalid", 443))
    assert result.state == "dns-error"


def test_check_port_uses_family_from_getaddrinfo(monkeypatch: pytest.MonkeyPatch) -> None:
    seen: list[int] = []

    monkeypatch.setattr(
        tcp.socket,
        "getaddrinfo",
        lambda *a, **k: [(socket.AF_INET6, socket.SOCK_STREAM, 6, "", ("::1", 5432, 0, 0))],
    )

    def fake_socket(family: int, *_args: Any, **_kwargs: Any) -> _FakeSocket:
        seen.append(family)
        return _FakeSocket(0)

    monkeypatch.setattr(tcp.socket, "socket", fake_socket)
    check_port(TcpTarget("::1", 5432))
    assert seen == [socket.AF_INET6]


@pytest.mark.integration
def test_check_port_real_refused_on_loopback() -> None:
    # Port 1 on loopback: nothing listens, and the kernel refuses immediately.
    result = check_port(TcpTarget("127.0.0.1", 1), timeout_s=2.0)
    assert result.state == "refused"
